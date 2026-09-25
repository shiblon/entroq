package subq

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// waitUntil polls cond until it holds or the test times out.
func waitUntil(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", desc)
		}
		time.Sleep(time.Millisecond)
	}
}

// startWaiters starts n Wait calls on qs, returning a channel that receives
// each one's result.
func startWaiters(ctx context.Context, s *SubQ, n int, qs ...string) <-chan error {
	done := make(chan error, n)
	for range n {
		go func() { done <- s.Wait(ctx, qs, 0, nil) }()
	}
	return done
}

func TestListenersTracksWaiters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New()

	if got := s.Listeners(); len(got) != 0 {
		t.Fatalf("new SubQ: want no waiters, got %v", got)
	}

	startWaiters(ctx, s, 2, "a")
	startWaiters(ctx, s, 1, "a", "b")
	want := map[string]int{"a": 3, "b": 1}
	waitUntil(t, "three waiters on a, one on b", func() bool {
		return cmp.Equal(want, s.Listeners())
	})

	cancel()
	waitUntil(t, "waiters to leave", func() bool {
		return len(s.Listeners()) == 0
	})
}

func TestNotifyWakesOneWaiter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New()

	done := startWaiters(ctx, s, 2, "q")
	waitUntil(t, "two waiters", func() bool { return s.Listeners()["q"] == 2 })

	s.Notify("q")
	if err := <-done; err != nil {
		t.Fatalf("woken waiter: %v", err)
	}
	// The other waiter must still be waiting, not woken by the same notice.
	select {
	case err := <-done:
		t.Fatalf("second waiter woke from one notification: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if got := s.Listeners()["q"]; got != 1 {
		t.Fatalf("remaining waiters: want 1, got %d", got)
	}
}

func TestNotifyWithoutWaitersIsDropped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New()

	s.Notify("q")
	done := startWaiters(ctx, s, 1, "q")
	waitUntil(t, "one waiter", func() bool { return s.Listeners()["q"] == 1 })

	// A notification sent before anyone waited must not be delivered later.
	select {
	case err := <-done:
		t.Fatalf("waiter woke from a notification sent before it waited: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWaitHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := New()

	done := startWaiters(ctx, s, 1, "q")
	waitUntil(t, "one waiter", func() bool { return s.Listeners()["q"] == 1 })
	cancel()
	if err := <-done; err == nil {
		t.Fatal("canceled wait: want error, got nil")
	}
}
