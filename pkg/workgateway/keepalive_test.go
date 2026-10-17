package workgateway

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// A request the gateway owes while it claims is answered with a ready once it
// has waited a third of the silence window, and the client answers that with a
// ready. With a 300ms lease the window is 600ms and a ready comes every 200ms.
const idleLease = 300 * time.Millisecond

// idleClient is a client whose messages are read as they arrive, so a test can
// wait for one with a deadline, or see that none came.
type idleClient struct {
	*client
	in chan *Response
}

// idleServed runs one session over pipes on an empty inbox, with idleLease.
func idleServed(ctx context.Context, t *testing.T) (*entroq.EntroQ, *idleClient, chan error) {
	t.Helper()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { eq.Close() })
	r, w, c := osPipes(t)
	ic := &idleClient{client: c, in: make(chan *Response, 16)}
	go func() {
		defer close(ic.in)
		for {
			var msg Response
			if err := c.dec.Decode(&msg); err != nil {
				return
			}
			ic.in <- &msg
		}
	}()
	return eq, ic, served(ctx, eq, r, w, WithLease(idleLease))
}

// recvBy is the client's next message, or nil if none arrives within d.
func recvBy(t *testing.T, c *idleClient, d time.Duration) *Response {
	t.Helper()
	select {
	case msg := <-c.in:
		return msg
	case <-time.After(d):
		return nil
	}
}

// openIdle registers on the inbox and says it is ready, returning the
// session.
func openIdle(t *testing.T, c *idleClient) string {
	t.Helper()
	c.send(&Request{Type: ReqConfig, Config: &Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}}})
	ready := recvBy(t, c, idleLease)
	if ready == nil {
		t.Fatal("config not answered")
	}
	expectType(t, ready, RespReady)
	c.send(&Request{Type: ReqReady, Session: ready.Session})
	return ready.Session
}

func TestIdleReadyKeepsAWaitingClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, _ := idleServed(ctx, t)
	session := openIdle(t, c)

	for i := 0; i < 3; i++ {
		start := time.Now()
		msg := recvBy(t, c, 2*idleLease)
		if msg == nil {
			t.Fatalf("idle turn %d: no ready within %v", i, 2*idleLease)
		}
		expectType(t, msg, RespReady)
		if waited := time.Since(start); waited < idleLease/2 {
			t.Errorf("idle turn %d: ready after %v, want about %v", i, waited, 2*idleLease/3)
		}
		c.send(&Request{Type: ReqReady, Session: session})
	}

	// A task is still the answer to whichever ready is owed.
	if _, err := eq.Modify(ctx, entroq.InsertingInto("inbox")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	for {
		msg := recvBy(t, c, 2*idleLease)
		if msg == nil {
			t.Fatal("no answer after the task arrived")
		}
		if msg.Type == RespWork {
			break
		}
		expectType(t, msg, RespReady)
		c.send(&Request{Type: ReqReady, Session: session})
	}
}

// TestTaskDuringAnIdleTurnWaitsForTheReady is a claim that lands while the
// client holds the turn: the task waits, and answers the client's ready when it
// comes.
func TestTaskDuringAnIdleTurnWaitsForTheReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, _ := idleServed(ctx, t)
	session := openIdle(t, c)

	expectType(t, recvBy(t, c, 2*idleLease), RespReady)
	if _, err := eq.Modify(ctx, entroq.InsertingInto("inbox")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if msg := recvBy(t, c, idleLease); msg != nil {
		t.Fatalf("got %q while the client held the turn", msg.Type)
	}
	c.send(&Request{Type: ReqReady, Session: session})
	msg := recvBy(t, c, idleLease/3)
	if msg == nil {
		t.Fatalf("no answer within %v of the ready", idleLease/3)
	}
	expectType(t, msg, RespWork)
}

func TestIdleSilenceEndsTheSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := idleServed(ctx, t)
	openIdle(t, c)

	expectType(t, recvBy(t, c, 2*idleLease), RespReady)
	stopped(ctx, t, eq, done, "")
}

func TestIdleQuitDrains(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, c, done := idleServed(ctx, t)
	session := openIdle(t, c)

	expectType(t, recvBy(t, c, 2*idleLease), RespReady)
	c.send(&Request{Type: ReqReady, Session: session, Quit: true})
	msg := recvBy(t, c, idleLease)
	if msg == nil {
		t.Fatalf("no answer to the quit within %v", idleLease)
	}
	expectType(t, msg, RespQuit)
	stopped(ctx, t, eq, done, "")
}

func TestIdleTurnTakesOnlyAReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, c, done := idleServed(ctx, t)
	session := openIdle(t, c)

	expectType(t, recvBy(t, c, 2*idleLease), RespReady)
	c.send(&Request{Type: ReqTime, Session: session})
	msg := recvBy(t, c, idleLease)
	if msg == nil {
		t.Fatalf("no answer within %v", idleLease)
	}
	expectType(t, msg, RespError)
	if msg.Class != ExitCaller.String() {
		t.Errorf("class = %q, want %q", msg.Class, ExitCaller)
	}
	if err := <-done; err == nil {
		t.Error("ServeConn = nil, want the caller's fault")
	}
}

// TestHTTPIdleReadyAnswersTheWaitingPost is the reason for the idle ready: a
// POST waiting on a claim is answered, rather than held until something between
// client and gateway gives up on it.
func TestHTTPIdleReadyAnswersTheWaitingPost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eq.Close()
	h := NewServer(ctx, eq, WithLease(idleLease))
	defer h.Close()
	srv := httptest.NewServer(h)
	defer srv.Close()

	w := &httpWorker{t: t, url: srv.URL}
	w.register(&Config{Queues: []string{"inbox"}, ProtocolVersions: []int32{1}})
	for i := 0; i < 2; i++ {
		resp, _, err := w.tryPost(2*idleLease, &Request{Type: ReqReady})
		if err != nil {
			t.Fatalf("idle turn %d: %v", i, err)
		}
		expectType(t, resp, RespReady)
	}
}
