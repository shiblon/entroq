package workgateway

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/worker"
)

// startFixture stands a session up far enough to inspect what Start decided,
// over a conn that answers nothing. Start must not need the conn for anything
// but storing it: it RETURNS the check rather than sending it, or an HTTP
// handler would have to read its own send.
func startFixture(ctx context.Context, t *testing.T, conf *Config, options ...Option) (*Gateway, *fakeConn) {
	t.Helper()
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	c := &fakeConn{}
	g, check, err := Start(ctx, client, conf, c, options...)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if check == nil || check.Type != SendCheck {
		t.Fatalf("Start returned check %+v, want one of type %q", check, SendCheck)
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

// TestTheCheckReportsTheSettledTimeout is why the clamp is visible: a worker
// written against an hour that quietly got five minutes looks flaky rather than
// capped, and the client is the only one who can tell the difference.
func TestTheCheckReportsTheSettledTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()

	g, check, err := Start(ctx, client, &Config{
		Queues:           []string{"inbox"},
		ProtocolVersions: []int32{1},
		WorkTimeoutS:     3600,
	}, &fakeConn{}, WithMaxWorkTimeout(90*time.Second))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()

	if check.WorkTimeoutS != 90 {
		t.Errorf("check reports %ds, want the 90s it was capped to", check.WorkTimeoutS)
	}
	if g.workTimeout != 90*time.Second {
		t.Errorf("session holds %v, want 90s", g.workTimeout)
	}
}

// TestStartReturnsTheCheckRatherThanSendingIt is the property an HTTP
// transport depends on. The handler that calls Start is the one that would
// have to collect the send, so sending here would deadlock it on itself.
func TestStartReturnsTheCheckRatherThanSendingIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	g, c := startFixture(ctx, t, &Config{
		Queues:           []string{"inbox"},
		ProtocolVersions: []int32{1},
	})
	if len(c.sent) != 0 {
		t.Errorf("Start sent %d messages through the conn; it must hand the check back instead", len(c.sent))
	}
	if g.sessionID == "" {
		t.Error("Start minted no session id")
	}
	if g.claimant == "" {
		t.Error("Start minted no claimant")
	}
}

// TestStartNamesTheClaimantInTheCheck checks that the client is told which
// consumer holds its tasks. It is what identifies the holder in a stored task
// and in the gateway's metrics, so a client that cannot see it cannot find its
// own work.
func TestStartNamesTheClaimantInTheCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()

	g, check, err := Start(ctx, client, &Config{
		Queues:           []string{"inbox"},
		ProtocolVersions: []int32{1},
	}, &fakeConn{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer g.Close()
	if check.Claimant != g.claimant {
		t.Errorf("check names claimant %q, but the session holds as %q", check.Claimant, g.claimant)
	}
	if check.Session != g.sessionID {
		t.Errorf("check names session %q, but the session is %q", check.Session, g.sessionID)
	}
}

// TestStartMintsAClaimantUnlessTold covers the escape hatch and its default.
// One session is one consumer, named after itself, so several sessions on one
// EntroQ client never claim as each other.
func TestStartMintsAClaimantUnlessTold(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conf := &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}}
	a, _ := startFixture(ctx, t, conf)
	b, _ := startFixture(ctx, t, conf)
	if a.claimant == b.claimant {
		t.Errorf("two sessions share the claimant %q; doc set exclusion would stop working between them", a.claimant)
	}

	shared := &Config{
		Queues:                    []string{"inbox"},
		ProtocolVersions:          []int32{1},
		DangerousClaimantOverride: "eqmr/shared",
	}
	c, _ := startFixture(ctx, t, shared)
	if c.claimant != "eqmr/shared" {
		t.Errorf("claimant = %q, want the override", c.claimant)
	}
}

// TestStartRefusesWithAMessage covers the registrations this gateway will not
// serve. Each is answered rather than returned: a client that needs to hear
// something is owed an answer, not a dropped connection.
func TestStartRefusesWithAMessage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()

	for name, conf := range map[string]*Config{
		"no protocol versions": {Queues: []string{"inbox"}},
		"a protocol nobody speaks": {
			Queues:           []string{"inbox"},
			ProtocolVersions: []int32{99},
		},
		// Refused at config time rather than when the worker loop finds out,
		// which is the difference between an answer and a puzzle.
		"no queues": {ProtocolVersions: []int32{1}},
	} {
		t.Run(name, func(t *testing.T) {
			g, msg, err := Start(ctx, client, conf, &fakeConn{})
			if err != nil {
				t.Fatalf("Start returned an error for %s; a refusal is a message: %v", name, err)
			}
			if g != nil {
				g.Close()
				t.Fatalf("Start built a session for a config naming %s", name)
			}
			if msg == nil {
				t.Fatal("Start refused without saying why")
			}
			if msg.Class != ExitCaller.String() {
				t.Errorf("class = %q, want %q: a bad registration is the caller's fault", msg.Class, ExitCaller.String())
			}
			if msg.Message == "" {
				t.Error("the refusal carries no reason")
			}
		})
	}
}

// TestStartErrorsOnlyWhenItCannotReply is the other half of that contract: an
// error means no answer could be formed at all, which is the transport's own
// bug rather than anything a client did.
func TestStartErrorsOnlyWhenItCannotReply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()

	conf := &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}}
	for name, call := range map[string]func() (*Gateway, *SendMessage, error){
		"no client": func() (*Gateway, *SendMessage, error) { return Start(ctx, nil, conf, &fakeConn{}) },
		"no conn":   func() (*Gateway, *SendMessage, error) { return Start(ctx, client, conf, nil) },
		"no config": func() (*Gateway, *SendMessage, error) { return Start(ctx, client, nil, &fakeConn{}) },
	} {
		t.Run(name, func(t *testing.T) {
			g, msg, err := call()
			if err == nil {
				t.Fatalf("Start accepted %s", name)
			}
			// Nothing accompanies a non-nil error.
			if g != nil || msg != nil {
				t.Errorf("Start returned (%v, %v) alongside an error", g, msg)
			}
		})
	}
}
