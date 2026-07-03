package eqpg

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
)

// gcTestClient opens an EntroQ client against the shared test database with the
// GC loop interval overridden (via the unexported test knob), so tests control
// how fast -- or whether -- the background GC loop fires.
func gcTestClient(ctx context.Context, t *testing.T, gcInterval time.Duration) *entroq.EntroQ {
	t.Helper()
	client, err := entroq.New(ctx, Opener(pgHostPort,
		WithDB("postgres"), WithUsername("postgres"), WithPassword("password"),
		WithConnectAttempts(10), withGCInterval(gcInterval)))
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

	client := gcTestClient(ctx, t, 20*time.Millisecond)
	defer client.Close()

	eqtest.GCCollectsInLoop(ctx, t, client, "/test/gcloop")
}

// TestGCCollectOnce drives collectOnce directly (white-box) to pin the SQL
// semantics: due gc= tasks are collected; not-yet-due activation, future arrival
// (claimed-equivalent), and non-gc queues are left alone. The GC interval is set
// long so the background loop does not collect out from under the assertions.
func TestGCCollectOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Client for convenient inserts; backend handle for collectOnce. Both point
	// at the same DB with the auto-loop effectively disabled (1h interval).
	client := gcTestClient(ctx, t, time.Hour)
	defer client.Close()

	b, err := Open(ctx, pgHostPort,
		WithDB("postgres"), WithUsername("postgres"), WithPassword("password"),
		WithConnectAttempts(10), withGCInterval(time.Hour))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer b.Close()

	const p = "/test/collectonce"
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

	n, err := b.collectOnce(ctx, 100)
	if err != nil {
		t.Fatalf("collectOnce: %v", err)
	}
	if want := 2; n != want {
		t.Errorf("collectOnce deleted %d, want %d (co_due0, co_past)", n, want)
	}

	// Verify exactly the intended survivors remain.
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
