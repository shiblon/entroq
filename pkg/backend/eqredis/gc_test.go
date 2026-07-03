package eqredis

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
)

// gcTestClient opens a client against the shared test Redis with the GC loop
// interval overridden, so tests control how fast -- or whether -- it fires.
func gcTestClient(ctx context.Context, t *testing.T, gcInterval time.Duration) *entroq.EntroQ {
	t.Helper()
	client, err := entroq.New(ctx, Opener(WithAddr(redisAddr), withGCInterval(gcInterval)))
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	return client
}

// TestGCLoopCollects asserts the always-on backend GC loop actually RUNS: built
// with a short interval, it auto-collects a due gc= task with no manual trigger.
func TestGCLoopCollects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := gcTestClient(ctx, t, 50*time.Millisecond)
	defer client.Close()

	eqtest.GCCollectsInLoop(ctx, t, client, fmt.Sprintf("/redistest/gcloop/%s", client.GenID()))
}

// TestGCRemovesEmptyQueues covers the pre-existing empty-queue bookkeeping
// cleanup (gc()), which the GC-loop refactor moved into runGCLoop: after a
// queue's last task is deleted, the loop should drop its name from {eq}:qs. This
// path had no test before; it is added here since the refactor touched it.
func TestGCRemovesEmptyQueues(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := gcTestClient(ctx, t, 50*time.Millisecond)
	defer client.Close()

	// Inspection handle for the {eq}:qs bookkeeping set (dormant loop).
	insp, err := Open(ctx, WithAddr(redisAddr), withGCInterval(time.Hour))
	if err != nil {
		t.Fatalf("open inspector: %v", err)
	}
	defer insp.Close()

	queue := "/redistest/emptyqueue/" + client.GenID()
	resp, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithRawValue([]byte("{}"))))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Delete the only task, leaving the queue empty (but still listed in {eq}:qs
	// until the cleanup runs).
	if _, err := client.Modify(ctx, resp.InsertedTasks[0].Delete()); err != nil {
		t.Fatalf("delete: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		names, err := insp.client.SMembers(ctx, queuesKey).Result()
		if err != nil {
			t.Fatalf("smembers: %v", err)
		}
		found := false
		for _, n := range names {
			if n == queue {
				found = true
			}
		}
		if !found {
			return // empty queue was reaped from the bookkeeping set
		}
		if time.Now().After(deadline) {
			t.Fatalf("empty queue %q not removed from %s within deadline", queue, queuesKey)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestGCCollectOnce drives collectOnce directly (white-box) to pin the Redis
// semantics: due gc= tasks are collected; not-yet-due activation, future arrival
// (claimed-equivalent), and non-gc queues are left alone. Auto-loops are held off
// with a long interval; assertions are per-task-id so other queues in the shared
// Redis do not affect them.
func TestGCCollectOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := gcTestClient(ctx, t, time.Hour)
	defer client.Close()

	b, err := Open(ctx, WithAddr(redisAddr), withGCInterval(time.Hour))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer b.Close()

	p := "/redistest/collectonce/" + client.GenID()
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	futureGC := fmt.Sprintf("%s/c/gc=%d", p, future.Unix())

	cases := []struct {
		id, queue string
		at        time.Time
		collected bool
	}{
		{"co_due0", p + "/a/gc=0", past, true},       // always-active, arrived => collected
		{"co_past", p + "/b/gc=100", past, true},     // activation long past, arrived => collected
		{"co_future", futureGC, past, false},         // activation in the future => not due
		{"co_claimed", p + "/a/gc=0", future, false}, // arrival in the future => claimed-equivalent
		{"co_plain", p + "/plain", past, false},      // not a gc= queue
	}
	for _, c := range cases {
		if _, err := client.Modify(ctx, entroq.InsertingInto(c.queue,
			entroq.WithID(c.id), entroq.WithArrivalTime(c.at), entroq.WithRawValue([]byte("{}")))); err != nil {
			t.Fatalf("insert %s: %v", c.id, err)
		}
	}

	if _, err := b.collectOnce(ctx, 1000); err != nil {
		t.Fatalf("collectOnce: %v", err)
	}

	for _, c := range cases {
		got, err := client.Tasks(ctx, c.queue)
		if err != nil {
			t.Fatalf("tasks %q: %v", c.queue, err)
		}
		present := false
		for _, tk := range got {
			if tk.ID == c.id {
				present = true
			}
		}
		if c.collected && present {
			t.Errorf("%s should have been collected but is still present", c.id)
		}
		if !c.collected && !present {
			t.Errorf("%s should have survived but was collected", c.id)
		}
	}
}
