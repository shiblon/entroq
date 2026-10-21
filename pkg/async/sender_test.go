package async

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
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
