package workgateway

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// A drain is the operator's quit, the first half of Server.Shutdown: it stops
// each session as its client's quit would. A session waiting on a claim is
// answered with a quit at once; one with a task in hand finishes it first.

// drainServed runs one session over pipes against an EntroQ holding n tasks, on
// the returned server.
func drainServed(ctx context.Context, t *testing.T, n int) (*entroq.EntroQ, *client, chan error, *Server) {
	t.Helper()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { eq.Close() })
	for i := 0; i < n; i++ {
		if _, err := eq.Modify(ctx, entroq.InsertingInto("inbox")); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	srv := NewServer(ctx, eq)
	r, w, c := osPipes(t)
	done := make(chan error, 1)
	go func() { done <- srv.ServeConn(ctx, NewStreamConn(Joining(r, w))) }()
	return eq, c, done, srv
}

func TestDrainAnswersAnIdleSessionWithQuit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done, srv := drainServed(ctx, t, 0)

	c.send(&Request{Type: ReqConfig, Config: &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}}})
	check := c.recv()
	expectType(t, check, RespReady)
	c.send(&Request{Type: ReqReady, Session: check.Session})

	// The ready is parked on an empty queue; the drain is what answers it.
	srv.drain()
	expectType(t, c.recv(), RespQuit)
	stopped(ctx, t, eq, done, "")
}

func TestDrainFinishesTheTaskInHand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done, srv := drainServed(ctx, t, 2)

	session, work := open(t, c, &Config{})
	expectType(t, work, RespWork)
	srv.drain()
	c.send(&Request{Type: ReqModify, Session: session, Modification: deleting(work.Task)})
	expectType(t, c.recv(), RespQuit)
	if left := stopped(ctx, t, eq, done, work.Task.GetId()); left != nil {
		t.Error("the task in hand did not commit")
	}
}

func TestDrainRunsTheSuccessTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done, srv := drainServed(ctx, t, 2)

	session, work := open(t, c, &Config{WantSuccess: true})
	expectType(t, work, RespWork)
	srv.drain()
	c.send(&Request{Type: ReqModify, Session: session, Modification: deleting(work.Task)})
	expectType(t, c.recv(), RespSuccess)
	c.send(&Request{Type: ReqDone, Session: session})
	expectType(t, c.recv(), RespQuit)
	if left := stopped(ctx, t, eq, done, work.Task.GetId()); left != nil {
		t.Error("the task in hand did not commit")
	}
}

func TestDrainRefusesANewSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done, srv := drainServed(ctx, t, 1)

	srv.drain()
	c.send(&Request{Type: ReqConfig, Config: &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}}})
	expectType(t, c.recv(), RespQuit)
	stopped(ctx, t, eq, done, "")
}

// TestShutdownClosesWhatItCannotWaitFor is a drain that runs out of time: the
// task in hand is never answered, so Shutdown closes the session and leaves the
// task to its lease.
func TestShutdownClosesWhatItCannotWaitFor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done, srv := drainServed(ctx, t, 1)

	_, work := open(t, c, &Config{})
	expectType(t, work, RespWork)
	sctx, scancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer scancel()
	if err := srv.Shutdown(sctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want %v", err, context.DeadlineExceeded)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session outlived its Shutdown")
	}
	tasks, err := eq.Tasks(ctx, "inbox")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != work.Task.GetId() || !tasks[0].At.After(time.Now()) {
		t.Errorf("inbox = %v, want the task in hand, still under its lease", tasks)
	}
}

// TestHTTPDrainWaitsForTheTaskInHand is the drain a server does on a signal:
// an idle session is answered with a quit, a busy one finishes, and Shutdown
// returns only once both have ended.
func TestHTTPDrainWaitsForTheTaskInHand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()
	for i := 0; i < 2; i++ {
		if _, err := eq.Modify(ctx, entroq.InsertingInto("inbox")); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	h := NewServer(ctx, eq)
	defer h.Close()
	srv := httptest.NewServer(h)
	defer srv.Close()

	conf := func() *Config { return &Config{Queues: []string{"idle"}, ProtocolVersions: []int32{1}} }
	idle := &httpWorker{t: t, url: srv.URL}
	idle.register(conf())
	idleAnswer := make(chan *Response, 1)
	go func() {
		resp, _ := idle.post(&Request{Type: ReqReady})
		idleAnswer <- resp
	}()

	busy := &httpWorker{t: t, url: srv.URL}
	c := conf()
	c.Queues = []string{"inbox"}
	busy.register(c)
	work := busy.ask(&Request{Type: ReqReady})
	expectType(t, work, RespWork)

	shutdown := make(chan error, 1)
	go func() { shutdown <- h.Shutdown(ctx) }()
	select {
	case resp := <-idleAnswer:
		if resp == nil || resp.Type != RespQuit {
			t.Fatalf("idle session answered %+v, want a quit", resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the drain did not answer the idle session")
	}

	select {
	case err := <-shutdown:
		t.Fatalf("Shutdown = %v before the task in hand was answered", err)
	case <-time.After(200 * time.Millisecond):
	}

	expectType(t, busy.ask(&Request{Type: ReqModify, Modification: deleting(work.Task)}), RespQuit)
	select {
	case err := <-shutdown:
		if err != nil {
			t.Errorf("Shutdown = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after every session ended")
	}

	tasks, err := eq.Tasks(ctx, "inbox")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID == work.Task.GetId() || tasks[0].Claims != 0 {
		t.Errorf("inbox = %v, want only the other task, never claimed", tasks)
	}
}
