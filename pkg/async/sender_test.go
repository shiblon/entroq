package async

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// serveSender runs s.Serve on a fresh listener and returns the listener's
// address and Serve's result.
func serveSender(ctx context.Context, t *testing.T, s *Sender) (string, chan error) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, lis) }()
	return lis.Addr().String(), done
}

// served waits for Serve's result.
func served(t *testing.T, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}

// TestSenderCloseBeforeServe is a Close that comes first, as when a sidecar
// stops before its sender has started: Close has nothing to shut down, and
// a Serve after it returns at once rather than serving with nobody to stop it.
func TestSenderCloseBeforeServe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := NewSender(nil, "127.0.0.1:0")
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	addr, done := serveSender(ctx, t, s)
	served(t, done)
	expectRefused(t, addr)
}

// expectRefused checks that nothing accepts connections on addr.
func expectRefused(t *testing.T, addr string) {
	t.Helper()
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Errorf("%s still accepts connections", addr)
	}
}

// TestSenderServeStopsWithItsContext is the context Serve was given ending: the
// server stops at once, and the listener with it.
func TestSenderServeStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sctx, stop := context.WithCancel(ctx)
	s := NewSender(nil, "127.0.0.1:0")
	addr, done := serveSender(sctx, t, s)

	// Serving: a request with no routable host is answered, not refused.
	res, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET while serving: %v", err)
	}
	res.Body.Close()

	stop()
	served(t, done)
	expectRefused(t, addr)
}

// TestSenderTerminalRequestAckReleasesTheBody is the receiver reporting that
// the request side failed while the session lives on: the request lane ends,
// and a body read blocked on the client is released then, not only when the
// session ends.
func TestSenderTerminalRequestAckReleasesTheBody(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("new EntroQ: %v", err)
	}
	defer eq.Close()

	const sessionID = "terminal-ack"
	body := newBlockingReadCloser()
	request := httptest.NewRequest(http.MethodPost, "http://service/upload", nil)
	request.Body = body
	requestAck := newReceiveLane("/service", sessionID, "request-ack", time.Now(), defaultLaneTiming)
	responseData := newReceiveLane("/service", sessionID, "response-data", time.Now(), defaultLaneTiming)
	session := newSenderSession(NewSender(eq, ""), httptest.NewRecorder(), request, sessionID, requestAck, responseData, nil, time.Now())

	go session.source.run(ctx)
	select {
	case <-body.readStarted:
	case <-ctx.Done():
		t.Fatal("the body was never read")
	}

	if _, err := eq.Modify(ctx, entroq.InsertingInto(requestAck.queue, entroq.WithValue(Envelope{
		FrameControl: FrameControl{Session: sessionID, Final: true, Error: "upstream request failed"},
	}))); err != nil {
		t.Fatalf("insert terminal ACK: %v", err)
	}
	if err := session.runRequestLane(ctx); err != nil {
		t.Fatalf("request lane = %v, want nil", err)
	}
	select {
	case <-body.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the terminal ACK did not release the blocked body read")
	}
	if ctx.Err() != nil {
		t.Fatal("the context ended; the release must not depend on it")
	}
}

// TestSenderLocalFailureNotifiesThePeer is the sender failing on its own
// account while its client is still there: a malformed response frame. The
// receiver is told at once, on its request queue, rather than left to wait
// out its liveness timeout.
func TestSenderLocalFailureNotifiesThePeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("new EntroQ: %v", err)
	}
	defer eq.Close()

	const sessionID = "local-failure"
	now := time.Now()
	requestAck := newReceiveLane("/service", sessionID, "request-ack", now, defaultLaneTiming)
	responseData := newReceiveLane("/service", sessionID, "response-data", now, defaultLaneTiming)
	peerRequest := newReceiveLane("/service", sessionID, "request-data", now, defaultLaneTiming)
	peerResponse := newReceiveLane("/service", sessionID, "response-ack", now, defaultLaneTiming)
	request := httptest.NewRequest(http.MethodGet, "http://service/thing", nil)
	session := newSenderSession(NewSender(eq, ""), httptest.NewRecorder(), request, sessionID, requestAck, responseData, nil, now)
	done := make(chan error, 1)
	go func() { done <- session.run(ctx) }()

	// The receiver's first ACK names its request queue; the empty body goes
	// out on it as the final request frame.
	if _, err := eq.Modify(ctx, entroq.InsertingInto(requestAck.queue, entroq.WithValue(Envelope{
		FrameControl: FrameControl{Session: sessionID, ReplyQueue: peerRequest.queue},
	}))); err != nil {
		t.Fatalf("insert request ACK: %v", err)
	}
	waitForTasks(ctx, t, eq, peerRequest.queue, 1)

	// Body before metadata is a protocol fault on this side.
	if _, err := eq.Modify(ctx, entroq.InsertingInto(responseData.queue, entroq.WithValue(Response{
		FrameControl: FrameControl{Session: sessionID, ReplyQueue: peerResponse.queue},
		Body:         []byte("no status first"),
	}))); err != nil {
		t.Fatalf("insert malformed response: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("session = nil, want the protocol fault")
		}
	case <-ctx.Done():
		t.Fatal("the session did not end on a malformed response")
	}
	if ctx.Err() != nil {
		t.Fatal("the client's context ended; the notice must not depend on it")
	}

	var notified bool
	for _, task := range waitForTasks(ctx, t, eq, peerRequest.queue, 2) {
		var env Envelope
		if err := json.Unmarshal(task.Value, &env); err != nil {
			t.Fatalf("decode %q: %v", peerRequest.queue, err)
		}
		if env.Final && env.Error != "" {
			notified = true
		}
	}
	if !notified {
		t.Error("the receiver's request queue has no final error notice")
	}
}

// waitForTasks returns queue's tasks once it holds at least n, or fails.
func waitForTasks(ctx context.Context, t *testing.T, eq *entroq.EntroQ, queue string, n int) []*entroq.Task {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		tasks, err := eq.Tasks(ctx, queue)
		if err != nil {
			t.Fatalf("tasks in %q: %v", queue, err)
		}
		if len(tasks) >= n {
			return tasks
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q holds %d tasks, want at least %d", queue, len(tasks), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
