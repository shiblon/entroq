package workgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// httpWorker is a worker over HTTP, written the way a foreign one would be: a
// loop, a switch, and a POST. It keeps one piece of state, the session id, and
// knows nothing about EntroQ.
type httpWorker struct {
	t       *testing.T
	url     string
	session string
}

// post carries one exchange and returns the status alongside the answer, since
// a status is part of what this transport tells a client. Every answer is a
// JSON Response, an error as much as anything else.
func (w *httpWorker) post(req *Request) (*Response, int) {
	w.t.Helper()
	req.Session = w.session
	body, err := json.Marshal(req)
	if err != nil {
		w.t.Fatalf("marshal %q: %v", req.Type, err)
	}
	res, err := http.Post(w.url, "application/json", bytes.NewReader(body))
	if err != nil {
		w.t.Fatalf("post %q: %v", req.Type, err)
	}
	defer res.Body.Close()
	return decodeAnswer(w.t, req.Type, res), res.StatusCode
}

// decodeAnswer is the JSON Response in res's body.
func decodeAnswer(t *testing.T, kind RequestType, res *http.Response) *Response {
	t.Helper()
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("answer to %q (status %d) has Content-Type %q, want application/json", kind, res.StatusCode, ct)
	}
	var resp Response
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("decode answer to %q: %v", kind, err)
	}
	return &resp
}

// ask is post for the usual case, where anything but 200 is a test failure.
func (w *httpWorker) ask(req *Request) *Response {
	w.t.Helper()
	resp, status := w.post(req)
	if status != http.StatusOK {
		w.t.Fatalf("%q answered with status %d: %+v", req.Type, status, resp)
	}
	return resp
}

// register opens a session and remembers its id, which is the one piece of
// state a client has to keep.
func (w *httpWorker) register(conf *Config) *Response {
	w.t.Helper()
	check := w.ask(&Request{Type: ReqConfig, Config: conf})
	w.session = check.Session
	return check
}

// httpFixture stands up a handler on a real server with a real EntroQ.
func httpFixture(ctx context.Context, t *testing.T, options ...Option) (*httpWorker, *entroq.EntroQ) {
	t.Helper()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { eq.Close() })

	// The handler's context is the SERVICE's, not any request's.
	h := NewServer(ctx, eq, options...)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return &httpWorker{t: t, url: srv.URL}, eq
}

// TestHTTPRunsOneTaskEndToEnd is the protocol over HTTP: one POST per exchange,
// nothing pushed, and one piece of client state.
func TestHTTPRunsOneTaskEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w, eq := httpFixture(ctx, t)
	if _, err := eq.Modify(ctx, entroq.InsertingInto("inbox",
		entroq.WithRawValue(json.RawMessage(`{"n":7}`)))); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	check := w.register(&Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}})
	if check.Type != RespReady {
		t.Fatalf("answer to config = %q, want %q", check.Type, RespReady)
	}
	if check.Session == "" || check.Claimant == "" {
		t.Fatalf("check names no session or claimant: %+v", check)
	}

	work := w.ask(&Request{Type: ReqReady})
	if work.Type != RespWork {
		t.Fatalf("answer to ready = %q, want %q", work.Type, RespWork)
	}
	if got := work.Task.GetValue().GetStructValue().GetFields()["n"].GetNumberValue(); got != 7 {
		t.Errorf("task value n = %v, want 7", got)
	}

	// Answer and drain in one message, which is why quit rides on a reply: the
	// modification commits before the session ends.
	resp, status := w.post(&Request{
		Type: ReqModify,
		Modification: &wireModReq{&pb.ModifyRequest{
			Deletes: []*pb.TaskID{{
				Queue:   work.Task.GetQueue(),
				Id:      work.Task.GetId(),
				Version: work.Task.GetVersion(),
			}},
		}},
		Quit: true,
	})
	// The session ends while answering, so either a last answer or a closed
	// session is correct here; what matters is what happened to the task.
	if status != http.StatusOK && status != http.StatusNotFound {
		t.Errorf("answer to the final modify had status %d: %+v", status, resp)
	}

	left, err := eq.Tasks(ctx, "inbox")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("inbox still holds %d tasks; the modification did not commit", len(left))
	}
}

// TestHTTPReadsRideBetweenTurns checks that a client may ask questions while it
// decides, each as its own POST, without losing the work it still owes an
// answer for.
func TestHTTPReadsRideBetweenTurns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w, eq := httpFixture(ctx, t)
	if _, err := eq.Modify(ctx,
		entroq.InsertingInto("inbox"),
		entroq.PuttingDocInto("cfg", entroq.WithKeys("limits", ""),
			entroq.WithRawContent(json.RawMessage(`{"max":5}`))),
	); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	w.register(&Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}})
	work := w.ask(&Request{Type: ReqReady})
	if work.Type != RespWork {
		t.Fatalf("got %q, want %q", work.Type, RespWork)
	}

	docs := w.ask(&Request{
		Type:      ReqDocs,
		DocsQuery: &wireDocsReq{&pb.DocsRequest{Query: &pb.DocQuery{Namespace: "cfg"}}},
	})
	if docs.Type != RespDocs || len(docs.Docs) != 1 {
		t.Fatalf("docs read = %q with %d docs, want %q with 1", docs.Type, len(docs.Docs), RespDocs)
	}
	queues := w.ask(&Request{Type: ReqQueues})
	if queues.Type != RespQueues {
		t.Fatalf("queues read = %q, want %q", queues.Type, RespQueues)
	}

	// Still the work turn.
	w.post(&Request{
		Type: ReqModify,
		Modification: &wireModReq{&pb.ModifyRequest{
			Deletes: []*pb.TaskID{{
				Queue:   work.Task.GetQueue(),
				Id:      work.Task.GetId(),
				Version: work.Task.GetVersion(),
			}},
		}},
		Quit: true,
	})

	left, err := eq.Tasks(ctx, "inbox")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("inbox still holds %d tasks after the commit", len(left))
	}
}

// TestHTTPSessionsAreSeparate checks the thing this transport exists for: many
// workers in one process, each its own session and its own consumer.
func TestHTTPSessionsAreSeparate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w, _ := httpFixture(ctx, t)
	conf := &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}}

	a := w.register(conf)
	b := (&httpWorker{t: t, url: w.url}).register(conf)

	if a.Session == b.Session {
		t.Errorf("two sessions share the id %q", a.Session)
	}
	if a.Claimant == b.Claimant {
		t.Errorf("two sessions share the claimant %q; doc set exclusion would stop working between them", a.Claimant)
	}
}

// TestHTTPUnknownSessionIsNotFound is the answer a client can act on: start
// over with a config. A 5xx would invite it to retry into the same wall.
func TestHTTPUnknownSessionIsNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w, _ := httpFixture(ctx, t)
	w.session = "a-session-that-never-was"
	resp, status := w.post(&Request{Type: ReqReady})
	if status != http.StatusNotFound {
		t.Errorf("unknown session answered with %d, want %d", status, http.StatusNotFound)
	}
	expectRefusal(t, resp, classGone)
}

// expectRefusal checks that resp is an error of class, with a message.
func expectRefusal(t *testing.T, resp *Response, class string) {
	t.Helper()
	if resp.Type != RespError || resp.Class != class || resp.Message == "" {
		t.Errorf("answer = %+v, want an error of class %q with a message", resp, class)
	}
}

// TestHTTPRefusesABadConfig checks that a registration this gateway will not
// serve is still answered, with a class a client can branch on and the status
// for it, and that it leaves no session behind.
func TestHTTPRefusesABadConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w, _ := httpFixture(ctx, t)
	resp, status := w.post(&Request{Type: ReqConfig, Config: &Config{
		Queues:           []string{"inbox"},
		ProtocolVersions: []int32{99},
	}})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if resp.Type != RespError {
		t.Errorf("refusal type = %q, want %q", resp.Type, RespError)
	}
	if resp.Class != ExitCaller.String() {
		t.Errorf("class = %q, want %q", resp.Class, ExitCaller.String())
	}
	if resp.Session != "" {
		t.Errorf("a refusal named session %q; there is no session to name", resp.Session)
	}
}

// TestHTTPForgetsFinishedSessions checks that a session which ended comes out
// of the map, so a late request is told to start over rather than waiting on
// channels nobody reads.
func TestHTTPForgetsFinishedSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()
	h := NewServer(ctx, eq)
	defer h.Close()
	srv := httptest.NewServer(h)
	defer srv.Close()

	w := &httpWorker{t: t, url: srv.URL}
	w.register(&Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}})
	if got := h.Sessions(); got != 1 {
		t.Fatalf("handler holds %d sessions, want 1", got)
	}

	// Quit with nothing in hand: there is no work to drain, so this is simply
	// goodbye.
	w.post(&Request{Type: ReqReady, Quit: true})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.Sessions() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := h.Sessions(); got != 0 {
		t.Errorf("handler still holds %d sessions after one ended", got)
	}
	if _, status := w.post(&Request{Type: ReqReady}); status != http.StatusNotFound {
		t.Errorf("a request for an ended session got %d, want %d", status, http.StatusNotFound)
	}
}

// TestHTTPRefusesWhatItDoesNotSpeak covers the two transport-level refusals.
// Nothing is known about the session yet, but each is still answered as an
// error a client reads the same way as any other: a caller fault.
func TestHTTPRefusesWhatItDoesNotSpeak(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	w, _ := httpFixture(ctx, t)

	res, err := http.Get(w.url)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET answered with %d, want %d", res.StatusCode, http.StatusMethodNotAllowed)
	}
	expectRefusal(t, decodeAnswer(t, "GET", res), ExitCaller.String())

	res, err = http.Post(w.url, "application/json", bytes.NewReader([]byte("not json")))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("a malformed body answered with %d, want %d", res.StatusCode, http.StatusBadRequest)
	}
	expectRefusal(t, decodeAnswer(t, "not json", res), ExitCaller.String())
}

// TestHTTPStatusFollowsTheClass pins the status each error class is answered
// with, so a client or a proxy that reads only the status reads it rightly.
func TestHTTPStatusFollowsTheClass(t *testing.T) {
	for _, tc := range []struct {
		resp *Response
		want int
	}{
		{&Response{Type: RespWork}, http.StatusOK},
		{&Response{Type: RespQuit}, http.StatusOK},
		{&Response{Type: RespError, Class: ExitCaller.String()}, http.StatusBadRequest},
		{&Response{Type: RespError, Class: ExitTransient.String()}, http.StatusServiceUnavailable},
		{&Response{Type: RespError, Class: classGone}, http.StatusNotFound},
		{&Response{Type: RespError, Class: ExitGateway.String()}, http.StatusInternalServerError},
		{&Response{Type: RespError}, http.StatusInternalServerError},
	} {
		if got := statusOf(tc.resp); got != tc.want {
			t.Errorf("statusOf(%s %q) = %d, want %d", tc.resp.Type, tc.resp.Class, got, tc.want)
		}
	}
}

// TestHTTPRefusesAnOversizeBody pins the bound both transports share: a body
// over the limit is 413, not the 400 that would read like a client's JSON was
// malformed.
//
// Driven straight at the handler rather than through a socket, because
// MaxBytesReader hangs the connection up as it refuses -- so a client still
// writing its body may see the reset rather than the status, and a test over a
// socket would be asserting on which of the two won.
func TestHTTPRefusesAnOversizeBody(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()

	const max = 8 << 10
	h := NewServer(ctx, eq, WithMaxFrameBytes(max))
	defer h.Close()

	// A config that opens with a string and never closes it, so the decoder
	// has to keep reading and the limit is what stops it.
	body := io.MultiReader(
		strings.NewReader(`{"type":"config","session":"`),
		&floodReader{remaining: 4 * max},
	)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", body))

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversize body answered with %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
	expectRefusal(t, decodeAnswer(t, ReqConfig, w.Result()), ExitCaller.String())
	if h.Sessions() != 0 {
		t.Errorf("a refused body left %d sessions behind, want none", h.Sessions())
	}
}

// TestHTTPWorkSurvivesDisconnection is what makes HTTP viable for long work.
//
// Once the gateway hands out a task the response completes and there is NO
// request in flight. The client can go away for as long as its work takes and
// come back with the answer, because DoWork is blocked on a channel read that
// has no opinion about connections, and the session's context is the service's
// rather than any request's.
//
// The lease here is far shorter than the gap, so the task is only still the
// client's if renewal kept running across it -- with nobody connected.
func TestHTTPWorkSurvivesDisconnection(t *testing.T) {
	const lease = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	backend, err := eqmem.Opener()(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	shared := func(context.Context) (entroq.Backend, error) { return backend, nil }
	eq, err := entroq.New(ctx, shared)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()
	intruder, err := entroq.New(ctx, shared, entroq.WithClaimantID("intruder"))
	if err != nil {
		t.Fatalf("New intruder: %v", err)
	}

	h := NewServer(ctx, eq, WithLease(lease))
	defer h.Close()
	srv := httptest.NewServer(h)
	defer srv.Close()

	if _, err := eq.Modify(ctx, entroq.InsertingInto("inbox")); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	w := &httpWorker{t: t, url: srv.URL}
	w.register(&Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}})
	work := w.ask(&Request{Type: ReqReady})
	if work.Type != RespWork {
		t.Fatalf("got %q, want %q", work.Type, RespWork)
	}

	// Nothing is connected now. Wait out several leases.
	time.Sleep(5 * lease)

	if got := h.Sessions(); got != 1 {
		t.Errorf("handler holds %d sessions across the gap, want 1", got)
	}
	// Renewal held the task the whole time, with no request in flight.
	if task, err := intruder.TryClaim(ctx, entroq.From("inbox")); err != nil {
		t.Fatalf("Intruder claim: %v", err)
	} else if task != nil {
		t.Error("another claimant took the task while its worker was disconnected")
	}

	// And the answer still lands, on the turn it was always owed.
	w.post(&Request{
		Type: ReqModify,
		Modification: &wireModReq{&pb.ModifyRequest{
			Deletes: []*pb.TaskID{{
				Queue:   work.Task.GetQueue(),
				Id:      work.Task.GetId(),
				Version: work.Task.GetVersion(),
			}},
		}},
		Quit: true,
	})
	left, err := eq.Tasks(ctx, "inbox")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("inbox still holds %d tasks; the answer after reconnecting did not commit", len(left))
	}
}

// tryPost is post with a deadline and without the fatal. A request that never
// completes is a result some tests need to see rather than hang on.
func (w *httpWorker) tryPost(timeout time.Duration, req *Request) (*Response, int, error) {
	w.t.Helper()
	req.Session = w.session
	body, err := json.Marshal(req)
	if err != nil {
		w.t.Fatalf("marshal %q: %v", req.Type, err)
	}
	hc := &http.Client{Timeout: timeout}
	res, err := hc.Post(w.url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	if res.StatusCode == 499 {
		// The gateway saw this client give up; there is no body.
		return nil, res.StatusCode, nil
	}
	return decodeAnswer(w.t, req.Type, res), res.StatusCode, nil
}

// TestHTTPWorkTimeoutEndsTheSession pins what a fired work timeout owes the
// client.
//
// The gateway gives up on a task whose answer never came and goes around for
// another one, but the client is still out there working. Their states have
// diverged with the client unable to find out, so the session must end: its
// next request is then a routing failure, which is the one signal that tells a
// client to stop.
func TestHTTPWorkTimeoutEndsTheSession(t *testing.T) {
	const budget = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w, eq := httpFixture(ctx, t, WithMaxWorkTimeout(budget))
	// Two tasks, so the gateway has another to claim the moment it gives up.
	for i := 0; i < 2; i++ {
		if _, err := eq.Modify(ctx, entroq.InsertingInto("inbox")); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	w.register(&Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}})
	if work := w.ask(&Request{Type: ReqReady}); work.Type != RespWork {
		t.Fatalf("got %q, want %q", work.Type, RespWork)
	}

	// Outlast the budget without answering, the way a client whose work ran
	// long does.
	time.Sleep(3 * budget)

	// A read stands in for the heartbeat: valid in any phase, and all a client
	// has to ask with.
	_, status, err := w.tryPost(2*time.Second, &Request{Type: ReqTime})
	if err != nil {
		t.Fatalf("the client's request never completed: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("got status %d, want %d: a client whose work timed out must learn its session is gone", status, http.StatusNotFound)
	}
}

// waitForSessions polls until h holds want sessions, and reports whether it got
// there before the deadline.
func waitForSessions(h *Server, want int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if h.Sessions() == want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return h.Sessions() == want
}

// TestHTTPIdleHangUpStopsClaiming is the HTTP form of a client leaving between
// tasks. A ready parked on an empty queue is a connection held open by the
// client, and a client that drops it must not leave a session claiming on its
// behalf.
func TestHTTPIdleHangUpStopsClaiming(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()
	h := NewServer(ctx, eq)
	defer h.Close()
	srv := httptest.NewServer(h)
	defer srv.Close()

	w := &httpWorker{t: t, url: srv.URL}
	w.register(&Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}})
	if _, _, err := w.tryPost(200*time.Millisecond, &Request{Type: ReqReady}); err == nil {
		t.Fatal("a ready on an empty queue was answered; it should have waited")
	}

	if !waitForSessions(h, 0, 5*time.Second) {
		t.Fatal("the session is still claiming for a client that hung up")
	}
	if _, status := w.post(&Request{Type: ReqReady}); status != http.StatusNotFound {
		t.Errorf("a request after the hang-up got %d, want %d", status, http.StatusNotFound)
	}
}

// TestHTTPHangUpAfterAnswerStillCommits pins the other side of a hang-up: a
// client that sends its answer and stops waiting for the reply still gets its
// answer committed, because the gateway had it before the client left.
//
// The commit is held open across the hang-up, so a fix that cancelled the whole
// session cannot pass by finishing the commit first.
func TestHTTPHangUpAfterAnswerStillCommits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	backend, err := eqmem.Opener()(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	release := make(chan struct{})
	eq, err := entroq.New(ctx, func(context.Context) (entroq.Backend, error) {
		return heldCommits{Backend: backend, release: release}, nil
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()
	seed, err := entroq.New(ctx, func(context.Context) (entroq.Backend, error) { return backend, nil })
	if err != nil {
		t.Fatalf("New seed: %v", err)
	}
	if _, err := seed.Modify(ctx, entroq.InsertingInto("inbox")); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	h := NewServer(ctx, eq)
	defer h.Close()
	srv := httptest.NewServer(h)
	defer srv.Close()

	w := &httpWorker{t: t, url: srv.URL}
	w.register(&Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}})
	h.mu.Lock()
	sess := h.sessions[w.session]
	h.mu.Unlock()
	work := w.ask(&Request{Type: ReqReady})
	if work.Type != RespWork {
		t.Fatalf("got %q, want %q", work.Type, RespWork)
	}

	_, _, err = w.tryPost(200*time.Millisecond, &Request{
		Type: ReqModify,
		Modification: &wireModReq{&pb.ModifyRequest{
			Deletes: []*pb.TaskID{{
				Queue:   work.Task.GetQueue(),
				Id:      work.Task.GetId(),
				Version: work.Task.GetVersion(),
			}},
		}},
	})
	if err == nil {
		t.Fatal("the modify was answered while its commit was held")
	}
	// The server learns of the dropped connection on its own schedule. Let
	// the commit through only once the session has heard, or has ended.
	select {
	case <-sess.conn.Gone():
	case <-sess.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session never heard the client hang up")
	}
	close(release)

	if !waitForSessions(h, 0, 5*time.Second) {
		t.Fatal("the session outlived a client that hung up")
	}
	left, err := seed.Tasks(ctx, "inbox")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("inbox still holds %d tasks; the answer sent before the hang-up did not commit", len(left))
	}
}
