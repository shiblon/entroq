package workgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/worker"
)

// These tests drive the gateway Bridge against a real in-memory EntroQ while the
// test itself plays the worker on the other end of an in-memory pipe. Because
// the test is in package workgateway, it builds and reads the wire types
// directly, so the assertions double as documentation of the exact protocol.
// Registration is out-of-band (a Config passed to Run), so the tests never send
// a wire register message.

// codec is the worker side of the wire in a test: send a message, receive the
// next one.
type codec struct {
	t   *testing.T
	enc *json.Encoder
	dec *json.Decoder
}

func (c *codec) send(v any) {
	c.t.Helper()
	if err := c.enc.Encode(v); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

func (c *codec) recv(v any) {
	c.t.Helper()
	if err := c.dec.Decode(v); err != nil {
		c.t.Fatalf("recv: %v", err)
	}
}

// session runs a Bridge over pipes against eq with the given registration, and
// the test plays the worker.
type session struct {
	t      *testing.T
	c      *codec
	cancel context.CancelFunc
	errc   chan error
}

func newSession(t *testing.T, ctx context.Context, eq *entroq.EntroQ, cfg Config, lease time.Duration) *session {
	t.Helper()
	clientR, gatewayW := io.Pipe()
	gatewayR, clientW := io.Pipe()
	bridge := NewBridge(NewPipeConn(gatewayR, gatewayW))

	rctx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- bridge.Run(rctx, eq, cfg, lease) }()

	return &session{
		t:      t,
		c:      &codec{t: t, enc: json.NewEncoder(clientW), dec: json.NewDecoder(clientR)},
		cancel: cancel,
		errc:   errc,
	}
}

// stop cancels the bridge and asserts it exited cleanly (nil or a cancellation).
// Use it for tests where the loop keeps running until the test is done with it.
func (s *session) stop() {
	s.t.Helper()
	s.cancel()
	if err := <-s.errc; err != nil && !errors.Is(err, context.Canceled) {
		s.t.Errorf("bridge run: %v", err)
	}
}

// wait returns the bridge's Run error, for tests where the worker is expected to
// stop on its own (a fatal or a bad registration).
func (s *session) wait() error {
	s.t.Helper()
	select {
	case err := <-s.errc:
		return err
	case <-time.After(5 * time.Second):
		s.t.Fatal("bridge did not exit on its own")
		return nil
	}
}

func newEQ(t *testing.T, ctx context.Context) *entroq.EntroQ {
	t.Helper()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	t.Cleanup(func() { eq.Close() })
	return eq
}

func insertTask(t *testing.T, ctx context.Context, eq *entroq.EntroQ, q, value string) {
	t.Helper()
	if _, err := eq.Modify(ctx, entroq.InsertingInto(q, entroq.WithValue(value))); err != nil {
		t.Fatalf("insert into %q: %v", q, err)
	}
}

// workCfg is the common registration: serve "in" with a work handler.
func workCfg(queues ...string) Config {
	if len(queues) == 0 {
		queues = []string{"in"}
	}
	return Config{Queues: queues, Work: true}
}

// TestBridge_OKDeletes is the happy path: receive the task, reply ok with a
// delete of the claimed task, and confirm the queue drains.
func TestBridge_OKDeletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	if dw.Type != msgDoWork {
		t.Fatalf("got %q, want %q", dw.Type, msgDoWork)
	}
	if got := string(dw.Task.Value); got != `"hello"` {
		t.Errorf("task value = %s, want %q", got, `"hello"`)
	}
	s.c.send(result{
		Type:         msgResult,
		Outcome:      outcomeOK,
		Modification: &modification{Deletes: []taskRef{{ID: dw.Task.ID, Version: dw.Task.Version}}},
	})

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("wait queue empty: %v", err)
	}
	s.stop()
}

// TestBridge_OKEmptyDoesNotDelete locks in the core faithfulness rule: "ok" means
// only "no error". With an empty modification the claimed task is committed
// unchanged and therefore stays in its queue (leased until expiry), exactly as a
// Go DoModify that returns no mods.
func TestBridge_OKEmptyDoesNotDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(result{Type: msgResult, Outcome: outcomeOK}) // no modification

	// Give the empty commit time to happen, then confirm the task is still there.
	time.Sleep(200 * time.Millisecond)
	tasks, err := eq.Tasks(ctx, "in")
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("queue has %d tasks, want 1: an empty ok must not delete the task", len(tasks))
	}
	s.stop()
}

// TestBridge_Insert covers producing work: the result inserts a new task into
// another queue and deletes the input in one atomic modification.
func TestBridge_Insert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(result{
		Type:    msgResult,
		Outcome: outcomeOK,
		Modification: &modification{
			Inserts: []insertArg{{Queue: "out", Value: json.RawMessage(`"produced"`)}},
			Deletes: []taskRef{{ID: dw.Task.ID, Version: dw.Task.Version}},
		},
	})

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("wait input drained: %v", err)
	}
	out, err := eq.Tasks(ctx, "out")
	if err != nil {
		t.Fatalf("tasks out: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("out has %d tasks, want 1", len(out))
	}
	if got := string(out[0].Value); got != `"produced"` {
		t.Errorf("produced value = %s, want %q", got, `"produced"`)
	}
	s.stop()
}

// TestBridge_TakeDocs exercises the optional doc-claim phase: the worker asks for
// a doc, the gateway claims it and passes it into doWork.
func TestBridge_TakeDocs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	if _, err := eq.Modify(ctx,
		entroq.InsertingInto("in", entroq.WithValue("hello")),
		entroq.InsertingDoc(&entroq.DocData{Namespace: "ns", Key: "k", Content: json.RawMessage(`"docval"`)}),
	); err != nil {
		t.Fatalf("insert task+doc: %v", err)
	}

	cfg := workCfg()
	cfg.TakeDocs = true
	s := newSession(t, ctx, eq, cfg, time.Second)

	var td takeDocsMsg
	s.c.recv(&td)
	if td.Type != msgTakeDocs {
		t.Fatalf("got %q, want %q", td.Type, msgTakeDocs)
	}
	s.c.send(docsMsg{Type: msgDocs, Claims: []docClaim{{Namespace: "ns", Key: "k"}}})

	var dw doWorkMsg
	s.c.recv(&dw)
	if len(dw.Docs) != 1 {
		t.Fatalf("doWork carried %d docs, want 1", len(dw.Docs))
	}
	if dw.Docs[0].Key != "k" {
		t.Errorf("doc key = %q, want %q", dw.Docs[0].Key, "k")
	}
	s.c.send(result{
		Type:         msgResult,
		Outcome:      outcomeOK,
		Modification: &modification{Deletes: []taskRef{{ID: dw.Task.ID, Version: dw.Task.Version}}},
	})

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("wait queue empty: %v", err)
	}
	s.stop()
}

// TestBridge_Cleanup exercises the optional post-commit phase: after the commit,
// the gateway sends cleanup and the worker acknowledges with done.
func TestBridge_Cleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	cfg := workCfg()
	cfg.Cleanup = true
	s := newSession(t, ctx, eq, cfg, time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(result{
		Type:         msgResult,
		Outcome:      outcomeOK,
		Modification: &modification{Deletes: []taskRef{{ID: dw.Task.ID, Version: dw.Task.Version}}},
	})

	var cu cleanupMsg
	s.c.recv(&cu)
	if cu.Type != msgCleanup {
		t.Fatalf("got %q, want %q", cu.Type, msgCleanup)
	}
	// Cleanup runs after the commit, so the task is already gone.
	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("wait queue empty: %v", err)
	}
	s.c.send(done{Type: msgDone})
	s.stop()
}

// TestBridge_CleanupFatal proves a fatal reply to cleanup stops the worker, and
// only after the task has already committed.
func TestBridge_CleanupFatal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	cfg := workCfg()
	cfg.Cleanup = true
	s := newSession(t, ctx, eq, cfg, time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(result{
		Type:         msgResult,
		Outcome:      outcomeOK,
		Modification: &modification{Deletes: []taskRef{{ID: dw.Task.ID, Version: dw.Task.Version}}},
	})
	var cu cleanupMsg
	s.c.recv(&cu)
	s.c.send(done{Type: msgDone, Outcome: outcomeFatal, Message: "cleanup blew up"})

	if _, ok := worker.AsFatal(s.wait()); !ok {
		t.Fatal("expected a fatal error to stop the worker")
	}
	// The task committed before the fatal cleanup, so the queue is empty.
	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("task should have committed before the fatal: %v", err)
	}
}

// TestBridge_RetryMoves checks the retry outcome with quarantine: with
// maxAttempts=1 an orMove retry lands the task in the error queue on first
// failure.
func TestBridge_RetryMoves(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	cfg := workCfg()
	cfg.MaxAttempts = 1
	s := newSession(t, ctx, eq, cfg, time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(result{Type: msgResult, Outcome: outcomeRetry, Message: "please retry", OrMove: "dead"})

	deadline := time.After(5 * time.Second)
	for {
		tasks, err := eq.Tasks(ctx, "dead")
		if err != nil {
			t.Fatalf("tasks: %v", err)
		}
		if len(tasks) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("task never quarantined to the error queue")
		case <-time.After(20 * time.Millisecond):
		}
	}
	s.stop()
}

// TestBridge_Fatal checks that a fatal outcome from doWork stops the worker.
func TestBridge_Fatal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(result{Type: msgResult, Outcome: outcomeFatal, Message: "boom"})

	if _, ok := worker.AsFatal(s.wait()); !ok {
		t.Fatal("expected a fatal error to stop the worker")
	}
}

// TestBridge_RequiresQueue rejects a registration with no queues.
func TestBridge_RequiresQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)

	s := newSession(t, ctx, eq, Config{Work: true}, time.Second) // no queues
	if err := s.wait(); err == nil {
		t.Fatal("expected an error for a registration with no queues")
	}
}

// TestBridge_RequiresWorkHandler rejects a registration with no work handler:
// the gateway would have nothing to do.
func TestBridge_RequiresWorkHandler(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)

	s := newSession(t, ctx, eq, Config{Queues: []string{"in"}}, time.Second) // Work:false
	if err := s.wait(); err == nil {
		t.Fatal("expected an error for a registration with no work handler")
	}
}
