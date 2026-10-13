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

// trySend is send without the fatal, for a test that has to tell a session
// that is gone from a message that was refused.
func (w *client) trySend(v any) error {
	w.t.Helper()
	return w.enc.Encode(v)
}

func (w *client) recv() *Response {
	w.t.Helper()
	var msg Response
	if err := w.dec.Decode(&msg); err != nil {
		w.t.Fatalf("client recv: %v", err)
	}
	return &msg
}

// brokenReader gives up some bytes and then fails, the way a connection reset
// mid-frame does.
type brokenReader struct {
	data []byte
	err  error
}

func (r *brokenReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (r *brokenReader) Close() error { return nil }

// failingWriter accepts nothing, the way a stream whose peer is gone does.
type failingWriter struct{ err error }

func (w failingWriter) Write(p []byte) (int, error) { return 0, w.err }

func (w failingWriter) Close() error { return nil }

// TestStreamFailedSendEndsTheSession pins that a failed write is terminal, and
// that the session reports the write as the reason.
//
// writeFrame writes a frame and its newline in one Write, and io.Writer
// guarantees a short write comes with an error -- so a failed Send may have
// left a fragment with no terminator on the stream. That terminator is the
// only frame boundary there is, so anything written afterwards concatenates
// onto the fragment and the peer can never resynchronize. Recovery is not
// unreliable here, it is impossible, which is why the pump that owns the
// stream ends the session rather than handing the problem back to one caller.
func TestStreamFailedSendEndsTheSession(t *testing.T) {
	ctx := context.Background()
	gone := errors.New("broken pipe")

	// A reader that only ever blocks, so nothing but the write can end this.
	blocked, idle, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	defer idle.Close()

	c := NewStreamConn(Joining(blocked, failingWriter{err: gone}))
	defer c.Close()

	if err := c.Send(ctx, &Response{Type: RespCheck}); !errors.Is(err, gone) {
		t.Errorf("Send = %v, want the write error %v", err, gone)
	}
	select {
	case <-c.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the session never ended after its write failed")
	}
	if err := c.ending(); !errors.Is(err, gone) {
		t.Errorf("the session ended with %v, want the write error %v", err, gone)
	}
	if _, err := c.Recv(ctx); !errors.Is(err, gone) {
		t.Errorf("Recv after a failed write = %v, want the write error %v", err, gone)
	}
}

// discardWriter takes responses nowhere, for a test that only drives reads.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func (discardWriter) Close() error { return nil }

// TestStreamBrokenReadReportsTheRead pins which error ends a session when the
// stream breaks part way through a frame.
//
// A failed read leaves a fragment, not a frame. Decoding it either fails and
// buries the real cause under a parse error, or -- worse -- succeeds, and the
// gateway acts on a request from a stream whose state it cannot see. Only a
// hang-up is different, because a peer that closed without its last newline
// still said something whole.
func TestStreamBrokenReadReportsTheRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "fragment cannot parse", data: `{"type":"rea`},
		{name: "fragment happens to parse", data: `{"type":"ready"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reset := errors.New("connection reset by peer")
			c := NewStreamConn(Joining(&brokenReader{data: []byte(tc.data), err: reset}, discardWriter{}))
			defer c.Close()

			req, err := c.Recv(context.Background())
			if req != nil {
				t.Errorf("Recv handed over %q from a stream that broke mid-frame", req.Type)
			}
			if !errors.Is(err, reset) {
				t.Errorf("Recv = %v, want the read error %v to survive", err, reset)
			}
		})
	}
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
	go func() { out <- Serve(ctx, eq, NewStreamConn(Joining(r, w)), options...) }()
	return out
}

// TestStreamRunsOneTaskEndToEnd is the whole protocol over file descriptors: a
// client registers, is checked in, says it is ready, collects a task, answers
// with a modification, and the task is gone.
func TestStreamRunsOneTaskEndToEnd(t *testing.T) {
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

// TestStreamAnswersReadsMidTurn checks that a client may ask questions while it
// decides, as many as it likes, without consuming the turn it still owes.
func TestStreamAnswersReadsMidTurn(t *testing.T) {
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

// TestStreamHangUpFreesTheTask is the failure a pipe makes easiest to produce and
// the one that matters: the parent dies holding a task.
//
// Another claimant gets it once the lease lapses, because nothing is renewing
// it any more. No heartbeat was required of the client to make that true.
func TestStreamHangUpFreesTheTask(t *testing.T) {
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

// TestStreamRefusesABadConfig checks that a refused registration is answered and
// does not take the process down with it: Start hands back a refusal and NO
// session, which ServeRW has to notice rather than dereference.
func TestStreamRefusesABadConfig(t *testing.T) {
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

// oneStream is a single two-way stream that counts its closes and, as a real
// socket does, refuses the second one.
type oneStream struct {
	io.Reader
	io.Writer
	closes int
}

func (s *oneStream) Close() error {
	s.closes++
	if s.closes > 1 {
		return errors.New("use of closed network connection")
	}
	return nil
}

// halfStream is one direction of a joined stream, counting its closes.
type halfStream struct {
	closes int
	err    error
}

func (h *halfStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (h *halfStream) Write(p []byte) (int, error) { return len(p), nil }
func (h *halfStream) Close() error                { h.closes++; return h.err }

// TestStreamConnClosesItsStreamOnce pins what Joining exists for.
//
// A socket handed over as both halves was closed twice, so a clean shutdown
// reported "use of closed network connection": the one caller that really did
// close it was told it had failed. A stream arrives already joined, so the conn
// closes exactly one thing exactly once.
func TestStreamConnClosesItsStreamOnce(t *testing.T) {
	blocked, idle, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	defer idle.Close()

	sock := &oneStream{Reader: blocked, Writer: io.Discard}
	c := NewStreamConn(sock)
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v, want nil: closing one stream once is a clean shutdown", err)
	}
	if sock.closes != 1 {
		t.Errorf("the stream was closed %d times, want 1", sock.closes)
	}
}

// TestJoiningClosesBothHalves is the other side of that contract: a stream made
// of two is not closed until both are, and either one's failure is reported.
func TestJoiningClosesBothHalves(t *testing.T) {
	stuck := errors.New("reader would not close")
	r, w := &halfStream{err: stuck}, &halfStream{}

	j := Joining(r, w)
	if err := j.Close(); !errors.Is(err, stuck) {
		t.Errorf("Close = %v, want the reader's %v", err, stuck)
	}
	// Closing twice must not close either half twice, which would report
	// "already closed" for a teardown that was clean the first time.
	if err := j.Close(); !errors.Is(err, stuck) {
		t.Errorf("second Close = %v, want the same %v", err, stuck)
	}
	if r.closes != 1 || w.closes != 1 {
		t.Errorf("closed the reader %d times and the writer %d, want 1 each", r.closes, w.closes)
	}
}

// TestStreamConnCloseIsOneStep is the hazard a channel close has and an error
// return does not: there is no safe way to test a channel before closing it, so
// the lock has to make the test and the close a single step.
func TestStreamConnCloseIsOneStep(t *testing.T) {
	r, w, _ := osPipes(t)
	c := NewStreamConn(Joining(r, w))

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

// tryRecv is recv without the fatal, for a test that has to tell a closed
// session from a wrong answer.
func (w *client) tryRecv() (*Response, error) {
	w.t.Helper()
	var msg Response
	if err := w.dec.Decode(&msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// TestStreamWorkTimeoutEndsTheSession pins what a fired work timeout owes a client
// over a pipe, which is what it owes one over HTTP.
//
// The gateway gives up on a task whose answer never came while the client is
// still out there working, so their states have diverged with the client unable
// to find out. The task goes back with its attempt counted and the session
// ends, rather than the gateway going around for another one without it.
func TestStreamWorkTimeoutEndsTheSession(t *testing.T) {
	const budget = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()
	// Two tasks, so going around for another one is something this gateway
	// could do if it were going to.
	for range 2 {
		if _, err := eq.Modify(ctx, entroq.InsertingInto("inbox")); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	r, w, c := osPipes(t)
	done := served(ctx, eq, r, w, WithMaxWorkTimeout(budget))

	c.send(&Request{Type: ReqConfig, Config: &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}}})
	check := c.recv()
	if check.Type != RespCheck {
		t.Fatalf("first message = %q, want %q", check.Type, RespCheck)
	}
	c.send(&Request{Type: ReqReady, Session: check.Session})
	if work := c.recv(); work.Type != RespWork {
		t.Fatalf("after ready, got %q, want %q", work.Type, RespWork)
	}

	// Never answer, the way a client whose work ran long does. The budget
	// expiring is the whole signal: the session ends without the client having
	// to speak, which is what it could not do before.
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve = %v, want nil: a fired work timeout is a handled ending", err)
		}
	case <-ctx.Done():
		t.Fatal("the session never ended after its work timeout fired")
	}

	// The client learns by failing, which is the one signal that tells it to
	// stop. Whether the write or the read reports it depends on which it
	// reaches first, and either is the session being gone.
	if err := c.trySend(&Request{Type: ReqTime, Session: check.Session}); err == nil {
		if _, err := c.tryRecv(); err == nil {
			t.Error("the client was still served after its work timed out")
		}
	}

	// Dispose first, then exit: the task is back with its attempt counted, so a
	// task that reliably kills its workers still reaches the claim limit.
	left, err := eq.Tasks(ctx, "inbox")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(left) != 2 {
		t.Fatalf("inbox holds %d tasks, want 2: the timed-out task was not put back", len(left))
	}
	var attempted int
	for _, task := range left {
		if task.Attempt > 0 {
			attempted++
		}
	}
	if attempted != 1 {
		t.Errorf("%d tasks carry an attempt, want 1: a work timeout must count against its task", attempted)
	}
}
