package workgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// client is a worker on the other end of a connection, written the way a
// foreign one would be: a loop, a switch, and a write. It knows nothing about
// EntroQ, and it speaks first, about itself.
type client struct {
	t      *testing.T
	enc    *json.Encoder
	dec    *json.Decoder
	hangUp func()
}

func (w *client) send(v any) {
	w.t.Helper()
	if err := w.enc.Encode(v); err != nil {
		w.t.Fatalf("client send: %v", err)
	}
}

func (w *client) recv() *Response {
	w.t.Helper()
	var msg Response
	if err := w.dec.Decode(&msg); err != nil {
		w.t.Fatalf("client recv: %v", err)
	}
	return &msg
}

// osPipes wires a session to a client over REAL OS PIPES, which is what a
// spawned child actually gets. Two one-way pipes make the pair, exactly as exec
// would hand a child its stdin and stdout.
//
// Worth exercising rather than io.Pipe: io.Pipe is synchronous, so a Send
// blocks until the far end reads, while an OS pipe has a kernel buffer, so a
// Send returns with nobody having read it.
func osPipes(t *testing.T) (gwR io.ReadCloser, gwW io.WriteCloser, c *client) {
	t.Helper()
	gwIn, clientOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	clientIn, gwOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	c = &client{
		t:   t,
		enc: json.NewEncoder(clientOut),
		dec: json.NewDecoder(clientIn),
		// hangUp is what a dead process does to its pipes, which is the only
		// goodbye this protocol needs.
		hangUp: sync.OnceFunc(func() {
			clientOut.Close()
			clientIn.Close()
		}),
	}
	t.Cleanup(c.hangUp)
	return gwIn, gwOut, c
}

// served runs one session in the background over the given pipes.
func served(ctx context.Context, eq entroq.Client, r io.ReadCloser, w io.WriteCloser, options ...Option) chan error {
	out := make(chan error, 1)
	go func() { out <- Serve(ctx, eq, NewRWConn(r, w), options...) }()
	return out
}

// TestRWRunsOneTaskEndToEnd is the whole protocol over file descriptors: a
// client registers, is checked in, says it is ready, collects a task, answers
// with a modification, and the task is gone.
func TestRWRunsOneTaskEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()
	if _, err := eq.Modify(ctx, entroq.InsertingInto("inbox",
		entroq.WithRawValue(json.RawMessage(`{"n":1}`)))); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	r, w, c := osPipes(t)
	done := served(ctx, eq, r, w)

	// The client speaks first, about itself.
	c.send(&Request{Type: ReqConfig, Config: &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}}})

	check := c.recv()
	if check.Type != RespCheck {
		t.Fatalf("first message = %q, want %q", check.Type, RespCheck)
	}
	if check.Session == "" || check.Claimant == "" {
		t.Errorf("check names no session or claimant: %+v", check)
	}

	c.send(&Request{Type: ReqReady, Session: check.Session})

	work := c.recv()
	if work.Type != RespWork {
		t.Fatalf("after ready, got %q, want %q", work.Type, RespWork)
	}
	// The task crosses as protojson of api/entroq.proto, which is the promise:
	// a foreign client generates this type and hand-models nothing.
	if got := work.Task.GetValue().GetStructValue().GetFields()["n"].GetNumberValue(); got != 1 {
		t.Errorf("task value n = %v, want 1", got)
	}

	c.send(&Request{
		Type:    ReqModify,
		Session: check.Session,
		Modification: &wireModReq{&pb.ModifyRequest{
			// The queue is required: authorization is decided on it, so no
			// layer invents one.
			Deletes: []*pb.TaskID{{
				Queue:   work.Task.GetQueue(),
				Id:      work.Task.GetId(),
				Version: work.Task.GetVersion(),
			}},
		}},
		Quit: true,
	})

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve = %v, want nil after a quit", err)
		}
	case <-ctx.Done():
		t.Fatal("the session never ended after a quit")
	}

	left, err := eq.Tasks(ctx, "inbox")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("inbox still holds %d tasks; the modification did not commit", len(left))
	}
}

// TestRWAnswersReadsMidTurn checks that a client may ask questions while it
// decides, as many as it likes, without consuming the turn it still owes.
func TestRWAnswersReadsMidTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()
	if _, err := eq.Modify(ctx,
		entroq.InsertingInto("inbox"),
		entroq.PuttingDocInto("cfg", entroq.WithKeys("limits", ""),
			entroq.WithRawContent(json.RawMessage(`{"max":5}`))),
	); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	r, w, c := osPipes(t)
	done := served(ctx, eq, r, w)

	c.send(&Request{Type: ReqConfig, Config: &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}}})
	check := c.recv()
	c.send(&Request{Type: ReqReady, Session: check.Session})
	work := c.recv()
	if work.Type != RespWork {
		t.Fatalf("got %q, want %q", work.Type, RespWork)
	}

	// Two reads in a row, mid-turn.
	c.send(&Request{
		Type:      ReqDocs,
		Session:   check.Session,
		DocsQuery: &wireDocsReq{&pb.DocsRequest{Query: &pb.DocQuery{Namespace: "cfg"}}},
	})
	if docs := c.recv(); docs.Type != RespDocs || len(docs.Docs) != 1 {
		t.Fatalf("docs read = %q with %d docs, want %q with 1", docs.Type, len(docs.Docs), RespDocs)
	}
	c.send(&Request{Type: ReqQueues, Session: check.Session})
	if queues := c.recv(); queues.Type != RespQueues {
		t.Fatalf("queues read = %q, want %q", queues.Type, RespQueues)
	}

	// The work turn is still the work turn.
	c.send(&Request{
		Type:    ReqModify,
		Session: check.Session,
		Modification: &wireModReq{&pb.ModifyRequest{
			Deletes: []*pb.TaskID{{
				Queue:   work.Task.GetQueue(),
				Id:      work.Task.GetId(),
				Version: work.Task.GetVersion(),
			}},
		}},
		Quit: true,
	})
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve = %v, want nil", err)
		}
	case <-ctx.Done():
		t.Fatal("the session never ended")
	}
}

// TestRWHangUpFreesTheTask is the failure a pipe makes easiest to produce and
// the one that matters: the parent dies holding a task.
//
// Another claimant gets it once the lease lapses, because nothing is renewing
// it any more. No heartbeat was required of the client to make that true.
func TestRWHangUpFreesTheTask(t *testing.T) {
	const lease = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// One store, two clients: the second asks for the task the first was
	// serving, which is the only way to prove it was really let go.
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

	if _, err := eq.Modify(ctx, entroq.InsertingInto("inbox")); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	r, w, c := osPipes(t)
	done := served(ctx, eq, r, w, WithLease(lease))

	c.send(&Request{Type: ReqConfig, Config: &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}}})
	check := c.recv()
	c.send(&Request{Type: ReqReady, Session: check.Session})
	if work := c.recv(); work.Type != RespWork {
		t.Fatalf("got %q, want %q", work.Type, RespWork)
	}

	// The parent dies without answering.
	c.hangUp()

	select {
	case err := <-done:
		// Hanging up is how a client says goodbye, so it must not read as a
		// fault: ExitGateway would tell a supervisor to report a bug.
		if ee, ok := AsExit(err); ok && ee.Class == ExitGateway {
			t.Errorf("a hang-up was classified %v: %v", ee.Class, err)
		}
	case <-ctx.Done():
		t.Fatal("the session did not end when the pipes closed")
	}

	var claimed *entroq.Task
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		task, err := intruder.TryClaim(ctx, entroq.From("inbox"))
		if err != nil {
			t.Fatalf("Intruder claim: %v", err)
		}
		if task != nil {
			claimed = task
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if claimed == nil {
		t.Error("no other claimant could take the task after the client hung up")
	}
}

// TestRWRefusesABadConfig checks that a refused registration is answered and
// does not take the process down with it: Start hands back a refusal and NO
// session, which ServeRW has to notice rather than dereference.
func TestRWRefusesABadConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()

	r, w, c := osPipes(t)
	done := served(ctx, eq, r, w)
	c.send(&Request{Type: ReqConfig, Config: &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{99}}})

	msg := c.recv()
	if msg.Type != RespError {
		t.Errorf("refusal type = %q, want %q", msg.Type, RespError)
	}
	if msg.Class != ExitCaller.String() {
		t.Errorf("class = %q, want %q", msg.Class, ExitCaller.String())
	}
	if !strings.Contains(msg.Message, "99") {
		t.Errorf("refusal does not name what was asked for: %q", msg.Message)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Error("Serve returned nil for a refused config")
		}
	case <-ctx.Done():
		t.Fatal("Serve did not return after refusing")
	}
}

// TestRWConnCloseIsOneStep is the hazard a channel close has and an error
// return does not: there is no safe way to test a channel before closing it, so
// the lock has to make the test and the close a single step.
func TestRWConnCloseIsOneStep(t *testing.T) {
	r, w, _ := osPipes(t)
	c := NewRWConn(r, w)

	start := make(chan struct{})
	results := make(chan error, 8)
	for range 8 {
		go func() {
			<-start
			results <- c.Close()
		}()
	}
	close(start)

	var closed, already int
	for range 8 {
		switch err := <-results; {
		case err == nil:
			closed++
		case errors.Is(err, io.EOF):
			already++
		default:
			t.Errorf("Close = %v, want nil or io.EOF", err)
		}
	}
	if closed != 1 {
		t.Errorf("%d callers believed they closed it, want exactly 1", closed)
	}
	if already != 7 {
		t.Errorf("%d callers were told it was already closed, want 7", already)
	}

	// A closed conn carries nothing, and says so the same way twice.
	ctx := context.Background()
	if _, err := c.Recv(ctx); !errors.Is(err, io.EOF) {
		t.Errorf("Recv after Close = %v, want io.EOF", err)
	}
	if err := c.Send(ctx, &Response{Type: RespCheck}); !errors.Is(err, io.EOF) {
		t.Errorf("Send after Close = %v, want io.EOF", err)
	}
}
