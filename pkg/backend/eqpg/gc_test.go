package eqpg

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/queues"
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

// goDue derives, from the Go parser, whether a gc=-marked queue is collectable
// at now: present with a valid activation time that has passed. This is the Go
// side of the grammar the SQL gc_due function reimplements.
func goDue(qname string, now time.Time) bool {
	at, present, err := queues.GCActivation(qname)
	if err != nil || !present {
		return false
	}
	return !at.After(now)
}

// gcGrammarVectors pin the gc= grammar shared by the Go parser
// (queues.GCActivation) and the SQL gc_due function. Timestamps are far in the
// past or future so "due" is unambiguous regardless of the exact current time.
// This is the single source of truth guarding the two implementations against
// drift; a Python-side check will mirror it when the Python backend gains GC.
var gcGrammarVectors = []struct {
	queue string
	due   bool
}{
	{"/q/gc=0", true},                            // always on
	{"/q/gc=/leaf", true},                        // empty value => always on
	{"/q/gc=1", true},                            // unix seconds, 1970
	{"/q/gc=946684800", true},                    // unix seconds, 2000
	{"/q/gc=4102444800", false},                  // unix seconds, 2100 (future)
	{"/q/gc=2000-01-01T00:00:00Z", true},         // RFC3339 past
	{"/q/gc=2999-01-01T00:00:00Z", false},        // RFC3339 future
	{"/q/gc=2000-01-01T00:00:00.000Z", true},     // JS toISOString, past
	{"/q/gc=notatime", false},                    // malformed => never collect
	{"/q/gc=12.5", false},                        // decimal => malformed
	{"/a/gc=0/b/gc=2999-01-01T00:00:00Z", false}, // last wins: future
	{"/a/gc=2999-01-01T00:00:00Z/b/gc=0", true},  // last wins: always on
}

// TestGCGrammarGoMatchesSQL guards against drift between queues.GCActivation
// (Go) and entroq.gc_due (SQL): for every vector both must agree with each other
// and with the expected result.
func TestGCGrammarGoMatchesSQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	b, err := Open(ctx, pgHostPort,
		WithDB("postgres"), WithUsername("postgres"), WithPassword("password"),
		WithConnectAttempts(10), withGCInterval(time.Hour))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer b.Close()

	now := time.Now()
	for _, v := range gcGrammarVectors {
		var sqlDue bool
		if err := b.DB.QueryRowContext(ctx, "SELECT entroq.gc_due($1)", v.queue).Scan(&sqlDue); err != nil {
			t.Fatalf("gc_due(%q): %v", v.queue, err)
		}
		gDue := goDue(v.queue, now)
		if gDue != v.due {
			t.Errorf("%q: Go goDue=%v, want %v", v.queue, gDue, v.due)
		}
		if sqlDue != v.due {
			t.Errorf("%q: SQL gc_due=%v, want %v", v.queue, sqlDue, v.due)
		}
		if gDue != sqlDue {
			t.Errorf("%q: Go/SQL DISAGREE (go=%v sql=%v) -- grammar drift", v.queue, gDue, sqlDue)
		}
	}
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
