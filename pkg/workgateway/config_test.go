package workgateway

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/worker"
)

// startFixture builds a session from a registration, without serving it, so a
// test can ask what the registration decided.
func startFixture(ctx context.Context, t *testing.T, conf *Config, options ...Option) (*Gateway, *fakeConn) {
	t.Helper()
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	c := &fakeConn{}
	g, err := newGateway(client, &Request{Type: ReqConfig, Config: conf}, c, options...)
	if err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	t.Cleanup(g.Close)
	return g, c
}

// TestErrorQueueTemplateReachesTheWorker checks the plumbing that was missing:
// a client's error queue has to become the worker's ErrQMap, or a quarantined
// task lands somewhere the client never named.
func TestErrorQueueTemplateReachesTheWorker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("a template is honored", func(t *testing.T) {
		g, _ := startFixture(ctx, t, &Config{
			Queues:           []string{"inbox"},
			ErrorQueue:       "dead/{inbox}",
			ProtocolVersions: []int32{1},
		})
		if got := g.worker.ErrorQueueFor("inbox"); got != "dead/inbox" {
			t.Errorf("ErrorQueueFor(inbox) = %q, want dead/inbox", got)
		}
	})

	t.Run("a template with no inbox is one queue for all", func(t *testing.T) {
		g, _ := startFixture(ctx, t, &Config{
			Queues:           []string{"inbox"},
			ErrorQueue:       "everything/bad",
			ProtocolVersions: []int32{1},
		})
		for _, inbox := range []string{"inbox", "other"} {
			if got := g.worker.ErrorQueueFor(inbox); got != "everything/bad" {
				t.Errorf("ErrorQueueFor(%q) = %q, want everything/bad", inbox, got)
			}
		}
	})

	t.Run("no template is the worker default", func(t *testing.T) {
		g, _ := startFixture(ctx, t, &Config{
			Queues:           []string{"inbox"},
			ProtocolVersions: []int32{1},
		})
		if got := g.worker.ErrorQueueFor("inbox"); got != worker.DefaultErrQMap("inbox") {
			t.Errorf("ErrorQueueFor(inbox) = %q, want the worker default %q", got, worker.DefaultErrQMap("inbox"))
		}
	})
}

// TestOperatorOptionsDefaultToTheWorkers checks that an operator who sets
// nothing gets what a native worker would have done, rather than a zero that
// means something else entirely -- a zero work timeout means NO BOUND, which
// is the opposite of a default.
func TestOperatorOptionsDefaultToTheWorkers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conf := &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}}

	t.Run("defaults", func(t *testing.T) {
		g, _ := startFixture(ctx, t, conf)
		if g.opts.lease != entroq.DefaultClaimDuration {
			t.Errorf("lease = %v, want the client default %v", g.opts.lease, entroq.DefaultClaimDuration)
		}
		if g.workTimeout != worker.DefaultWorkTimeout {
			t.Errorf("workTimeout = %v, want the worker default %v", g.workTimeout, worker.DefaultWorkTimeout)
		}
	})

	t.Run("overridden", func(t *testing.T) {
		g, _ := startFixture(ctx, t, conf, WithLease(7*time.Second), WithMaxWorkTimeout(time.Hour))
		if g.opts.lease != 7*time.Second {
			t.Errorf("lease = %v, want 7s", g.opts.lease)
		}
		if g.workTimeout != time.Hour {
			t.Errorf("workTimeout = %v, want 1h", g.workTimeout)
		}
	})
}

// TestWorkTimeoutIsAskedForAndCapped covers the one setting both sides have a
// say in: the client knows how long its work takes, and the operator knows that
// an unbounded wait holds a task where no other worker can reach it.
func TestWorkTimeoutIsAskedForAndCapped(t *testing.T) {
	for name, tc := range map[string]struct {
		want, ceiling, settled time.Duration
	}{
		"asking for nothing takes the cap": {0, 5 * time.Minute, 5 * time.Minute},
		"asking for less than the cap":     {20 * time.Second, 5 * time.Minute, 20 * time.Second},
		"asking for more than the cap":     {time.Hour, 5 * time.Minute, 5 * time.Minute},
		"no cap honors whatever was asked": {time.Hour, 0, time.Hour},
		"no cap and no ask is no bound":    {0, 0, 0},
		"the cap is not a floor":           {time.Second, time.Hour, time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if got := workTimeout(tc.want, tc.ceiling); got != tc.settled {
				t.Errorf("workTimeout(%v, %v) = %v, want %v", tc.want, tc.ceiling, got, tc.settled)
			}
		})
	}
}

// session is a transport's side of a ChannelConn: the two ends the gateway does
// not hold, plus whatever Serve finally returned.
type session struct {
	t      *testing.T
	req    chan *Request
	resp   chan *Response
	cancel context.CancelFunc

	// done closes when Serve returns, and err is what it returned. A closed
	// channel can be waited on any number of times, which a one-shot send
	// cannot: a test that checks the outcome itself would leave the cleanup
	// waiting forever.
	done chan struct{}
	err  error
}

// wait returns what Serve returned, once it has.
func (s *session) wait() error {
	s.t.Helper()
	select {
	case <-s.done:
		return s.err
	case <-time.After(10 * time.Second):
		s.t.Fatal("Serve never returned")
		return nil
	}
}

// ask forwards one request and collects its response, which is one exchange of
// whatever a real transport is carrying them over.
func (s *session) ask(req *Request) *Response {
	s.t.Helper()
	select {
	case s.req <- req:
	case <-s.done:
		s.t.Fatalf("session ended before it took %q: %v", req.Type, s.err)
	case <-time.After(10 * time.Second):
		s.t.Fatalf("nobody took the %q request", req.Type)
	}
	select {
	case resp := <-s.resp:
		return resp
	case <-s.done:
		s.t.Fatalf("session ended before answering %q: %v", req.Type, s.err)
	case <-time.After(10 * time.Second):
		s.t.Fatalf("no answer to the %q request", req.Type)
	}
	return nil
}

// serveChannels runs a session the way an HTTP service would: unbuffered
// channels, Serve in a goroutine of its own, and the transport keeping only the
// two ends and the cancel.
func serveChannels(t *testing.T, eq entroq.Client, options ...Option) *session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{
		t:      t,
		req:    make(chan *Request),
		resp:   make(chan *Response),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	conn := NewChannelConn(s.req, s.resp)
	go func() {
		defer close(s.done)
		s.err = Serve(ctx, eq, conn, options...)
	}()
	t.Cleanup(func() {
		cancel()
		s.wait()
	})
	return s
}

func testClient(ctx context.Context, t *testing.T) *entroq.EntroQ {
	t.Helper()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { eq.Close() })
	return eq
}

// TestCheckAnswersTheConfig covers what a client cannot work out for itself and
// so must be told: which session it is, which consumer holds its tasks, and how
// long it actually gets for one of them.
func TestCheckAnswersTheConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := serveChannels(t, testClient(ctx, t))

	check := s.ask(&Request{Type: ReqConfig, Config: &Config{
		Queues:           []string{"inbox"},
		ProtocolVersions: []int32{1},
	}})
	if check.Type != RespCheck {
		t.Fatalf("answer to config = %q, want %q", check.Type, RespCheck)
	}
	if check.Session == "" {
		t.Error("check names no session, so a transport cannot register one")
	}
	if check.Claimant == "" {
		t.Error("check names no claimant, so a client cannot find its own work")
	}
}

// TestTheCheckReportsTheSettledTimeout is why the clamp is visible: a worker
// written against an hour that quietly got ninety seconds looks flaky rather
// than capped, and the client is the only one who can tell those apart.
func TestTheCheckReportsTheSettledTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := serveChannels(t, testClient(ctx, t), WithMaxWorkTimeout(90*time.Second))

	check := s.ask(&Request{Type: ReqConfig, Config: &Config{
		Queues:           []string{"inbox"},
		ProtocolVersions: []int32{1},
		WorkTimeoutS:     3600,
	}})
	if check.WorkTimeoutS != 90 {
		t.Errorf("check reports %ds, want the 90s it was capped to", check.WorkTimeoutS)
	}
}

// TestSessionsDoNotShareAClaimant is the safety the override exists to let you
// break deliberately: one session is one consumer, named after itself, so doc
// set exclusion keeps working between sessions on one EntroQ client.
func TestSessionsDoNotShareAClaimant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	eq := testClient(ctx, t)
	conf := &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}}

	a := serveChannels(t, eq).ask(&Request{Type: ReqConfig, Config: conf})
	b := serveChannels(t, eq).ask(&Request{Type: ReqConfig, Config: conf})
	if a.Claimant == b.Claimant {
		t.Errorf("two sessions share the claimant %q; doc set exclusion would stop working between them", a.Claimant)
	}
	if a.Session == b.Session {
		t.Errorf("two sessions share the id %q", a.Session)
	}

	shared := serveChannels(t, eq).ask(&Request{Type: ReqConfig, Config: &Config{
		Queues:                    []string{"inbox"},
		ProtocolVersions:          []int32{1},
		DangerousClaimantOverride: "eqmr/shared",
	}})
	if shared.Claimant != "eqmr/shared" {
		t.Errorf("claimant = %q, want the override", shared.Claimant)
	}
}

// TestServeRefusesWithAMessage covers the registrations this gateway will not
// serve. Each is answered rather than returned: a client that needs to hear
// something is owed an answer, not a dropped connection.
func TestServeRefusesWithAMessage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	eq := testClient(ctx, t)

	for name, req := range map[string]*Request{
		"no protocol versions": {Type: ReqConfig, Config: &Config{Queues: []string{"inbox"}}},
		"a protocol nobody speaks": {Type: ReqConfig, Config: &Config{
			Queues:           []string{"inbox"},
			ProtocolVersions: []int32{99},
		}},
		// Refused at config time rather than when the worker loop finds out,
		// which is the difference between an answer and a puzzle.
		"no queues":  {Type: ReqConfig, Config: &Config{ProtocolVersions: []int32{1}}},
		"no config":  {Type: ReqConfig},
		"not config": {Type: ReqReady},
	} {
		t.Run(name, func(t *testing.T) {
			s := serveChannels(t, eq)
			msg := s.ask(req)
			if msg.Type != RespError {
				t.Errorf("refusal type = %q, want %q", msg.Type, RespError)
			}
			if msg.Class != ExitCaller.String() {
				t.Errorf("class = %q, want %q: a bad registration is the caller's fault", msg.Class, ExitCaller.String())
			}
			if msg.Message == "" {
				t.Error("the refusal carries no reason")
			}
			// And the session is over, with the refusal already delivered.
			if err := s.wait(); err == nil {
				t.Error("Serve returned nil for a refused config")
			}
		})
	}
}

// TestServeErrorsOnlyWhenItCannotReply is the other half of that contract: an
// error means no answer could be formed at all, which is the transport's own
// bug rather than anything a client did.
func TestServeErrorsOnlyWhenItCannotReply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	eq := testClient(ctx, t)

	if err := Serve(ctx, nil, &fakeConn{}); err == nil {
		t.Error("Serve accepted a nil client")
	}
	if err := Serve(ctx, eq, nil); err == nil {
		t.Error("Serve accepted a nil conn")
	}
}
