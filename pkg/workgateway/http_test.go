package workgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
// a status is part of what this transport tells a client.
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
	if res.StatusCode != http.StatusOK {
		return nil, res.StatusCode
	}
	var resp Response
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		w.t.Fatalf("decode answer to %q: %v", req.Type, err)
	}
	return &resp, res.StatusCode
}

// ask is post for the usual case, where anything but 200 is a test failure.
func (w *httpWorker) ask(req *Request) *Response {
	w.t.Helper()
	resp, status := w.post(req)
	if resp == nil {
		w.t.Fatalf("%q answered with status %d", req.Type, status)
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
	h := NewHandler(ctx, eq, options...)
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
	if check.Type != RespCheck {
		t.Fatalf("answer to config = %q, want %q", check.Type, RespCheck)
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
	if resp == nil && status != http.StatusNotFound {
		t.Errorf("answer to the final modify had status %d", status)
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
	if _, status := w.post(&Request{Type: ReqReady}); status != http.StatusNotFound {
		t.Errorf("unknown session answered with %d, want %d", status, http.StatusNotFound)
	}
}

// TestHTTPRefusesABadConfig checks that a registration this gateway will not
// serve is still answered, with a class a client can branch on, and that it
// leaves no session behind.
func TestHTTPRefusesABadConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w, _ := httpFixture(ctx, t)
	resp := w.ask(&Request{Type: ReqConfig, Config: &Config{
		Queues:           []string{"inbox"},
		ProtocolVersions: []int32{99},
	}})
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
	h := NewHandler(ctx, eq)
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
	w.post(&Request{Type: ReqQuit})

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

// TestHTTPRefusesWhatItDoesNotSpeak covers the two transport-level refusals,
// which are statuses rather than protocol messages: nothing is known about the
// session yet, so there is nothing to answer in the protocol's own terms.
func TestHTTPRefusesWhatItDoesNotSpeak(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	w, _ := httpFixture(ctx, t)

	res, err := http.Get(w.url)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET answered with %d, want %d", res.StatusCode, http.StatusMethodNotAllowed)
	}

	res, err = http.Post(w.url, "application/json", bytes.NewReader([]byte("not json")))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("a malformed body answered with %d, want %d", res.StatusCode, http.StatusBadRequest)
	}
}
