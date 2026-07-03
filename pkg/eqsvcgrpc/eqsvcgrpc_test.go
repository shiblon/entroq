package eqsvcgrpc

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// waitFor polls cond until it is true or the deadline passes.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func queueSize(ctx context.Context, t *testing.T, eq *entroq.EntroQ, q string) int {
	t.Helper()
	sizes, err := eq.Queues(ctx)
	if err != nil {
		t.Fatalf("Queues: %v", err)
	}
	return sizes[q]
}

// gc=1 is a Unix timestamp of 1 second past the epoch: always due for GC.
const dueQueue = "/svc/gc=1"

func insertOne(ctx context.Context, t *testing.T, eq *entroq.EntroQ, q string) {
	t.Helper()
	if _, err := eq.Modify(ctx, entroq.InsertingInto(q, entroq.WithRawValue([]byte("{}")))); err != nil {
		t.Fatalf("insert into %q: %v", q, err)
	}
}

// TestQSvcGCEnabledDrains checks that WithGC starts a background loop that
// drains a due queue against the service's own backend, and that Close cancels
// and joins that loop (Close returning is proof the goroutine exited).
func TestQSvcGCEnabledDrains(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := New(ctx, eqmem.Opener(), WithGC(), WithGCInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	insertOne(ctx, t, svc.impl, dueQueue)

	if !waitFor(5*time.Second, func() bool { return queueSize(ctx, t, svc.impl, dueQueue) == 0 }) {
		t.Fatalf("queue %q was not collected by the background GC loop", dueQueue)
	}

	// Close cancels and joins the GC goroutine before closing the backend, so
	// it must return promptly. A hang here means cancellation is broken.
	done := make(chan error, 1)
	go func() { done <- svc.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5s: GC goroutine was not canceled")
	}
}

// TestQSvcGCDisabledByDefault checks that without WithGC the service starts no
// GC loop, so a due queue is left untouched.
func TestQSvcGCDisabledByDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := New(ctx, eqmem.Opener(), WithGCInterval(10*time.Millisecond)) // no WithGC
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer svc.Close()

	insertOne(ctx, t, svc.impl, dueQueue)

	// Well beyond several scan intervals, had a loop been running.
	time.Sleep(200 * time.Millisecond)
	if got := queueSize(ctx, t, svc.impl, dueQueue); got != 1 {
		t.Errorf("GC is off by default, but queue %q has %d task(s) (want 1)", dueQueue, got)
	}
}
