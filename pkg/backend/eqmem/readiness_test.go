package eqmem

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
)

func TestClaimUnblocksAtFutureArrival(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, Opener(WithReadinessInterval(10*time.Millisecond)))
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer client.Close()

	const queue = "/test/future-readiness"
	arrival := time.Now().Add(200 * time.Millisecond)
	resp, err := client.Modify(ctx, entroq.InsertingInto(
		queue,
		entroq.WithArrivalTime(arrival),
		entroq.WithValue("future"),
	))
	if err != nil {
		t.Fatalf("insert future task: %v", err)
	}

	// This cannot return through polling before the test context expires. The
	// readiness notification must wake it after the task's arrival time.
	claimed, err := client.Claim(
		ctx,
		entroq.From(queue),
		entroq.ClaimPollTime(time.Hour),
	)
	if err != nil {
		t.Fatalf("claim future task: %v", err)
	}
	if claimed.ID != resp.InsertedTasks[0].ID {
		t.Fatalf("claimed task %q, want %q", claimed.ID, resp.InsertedTasks[0].ID)
	}
	if now := time.Now(); now.Before(arrival) {
		t.Fatalf("claimed future task at %v before arrival %v", now, arrival)
	}
}

func TestReadinessFanout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const interval = 500 * time.Millisecond
	client, err := entroq.New(ctx, Opener(WithReadinessInterval(interval)))
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer client.Close()
	eqtest.ReadinessFanout(interval)(ctx, t, client, "/test/fanout")
}

func TestClaimHeapCountAvailable(t *testing.T) {
	now := time.Now()
	h := newClaimHeap()
	// Interleave available (even) and future (odd) items so both kinds end up
	// scattered through the heap.
	for i := range 12 {
		at := now.Add(-time.Duration(i) * time.Second)
		if i%2 == 1 {
			at = now.Add(time.Duration(i) * time.Second)
		}
		h.PushItem(newItem("q", fmt.Sprint(i), at))
	}
	var want int
	for _, item := range h.Items() {
		if !now.Before(item.at) {
			want++
		}
	}

	if got := h.CountAvailable(now, 100); got != want {
		t.Errorf("CountAvailable(no cap): got %d, want %d", got, want)
	}
	if got := h.CountAvailable(now, 3); got != 3 {
		t.Errorf("CountAvailable(cap 3): got %d, want 3", got)
	}
	if got := h.CountAvailable(now.Add(-time.Hour), 100); got != 0 {
		t.Errorf("CountAvailable(before everything): got %d, want 0", got)
	}
	var empty *claimHeap
	if got := empty.CountAvailable(now, 10); got != 0 {
		t.Errorf("CountAvailable(nil heap): got %d, want 0", got)
	}
}
