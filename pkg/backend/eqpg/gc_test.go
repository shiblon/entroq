package eqpg

import (
	"context"
	"database/sql"
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

// goActivation derives, from the Go parser, a gc=-marked queue's observable
// state: malformed (present but unparseable) and, when well-formed, whether it is
// due at now. This is the Go side of the grammar the SQL gc_activation function
// reimplements.
func goActivation(qname string, now time.Time) (malformed, due bool) {
	at, present, err := queues.GCActivation(qname)
	if !present {
		return false, false
	}
	if err != nil {
		return true, false // opted in but unparseable
	}
	return false, !at.After(now)
}

// gcGrammarVectors pin the gc= grammar shared by the Go parser
// (queues.GCActivation) and the SQL gc_activation function. Timestamps are far in
// the past or future so "due" is unambiguous regardless of the exact current
// time. This is the single source of truth guarding the two implementations
// against drift; the Python client relies on the same SQL, so it needs no parser.
var gcGrammarVectors = []struct {
	queue          string
	malformed, due bool
}{
	{"/q/gc=0", false, true},                            // always on
	{"/q/gc=/leaf", false, true},                        // empty value => always on
	{"/q/gc=1", false, true},                            // unix seconds, 1970
	{"/q/gc=946684800", false, true},                    // unix seconds, 2000
	{"/q/gc=4102444800", false, false},                  // unix seconds, 2100 (future)
	{"/q/gc=2000-01-01T00:00:00Z", false, true},         // RFC3339 past
	{"/q/gc=2999-01-01T00:00:00Z", false, false},        // RFC3339 future
	{"/q/gc=2000-01-01T00:00:00.000Z", false, true},     // JS toISOString, past
	{"/q/gc=2000-01-01T00:00:00+00:00", false, true},    // Python aware isoformat, past
	{"/q/gc=notatime", true, false},                     // malformed => never collect
	{"/q/gc=12.5", true, false},                         // decimal => malformed
	{"/q/gc=2000-01-01 00:00:00Z", true, false},         // space separator => malformed (strict)
	{"/q/gc=2000-01-01", true, false},                   // bare date => malformed (strict)
	{"/q/gc=2000-01-01T00:00:00", true, false},          // no timezone => malformed (strict)
	{"/q/gc=2000-01-01T00:00:00+0000", true, false},     // colonless offset => malformed (strict)
	{"/a/gc=0/b/gc=2999-01-01T00:00:00Z", false, false}, // last wins: future
	{"/a/gc=2999-01-01T00:00:00Z/b/gc=0", false, true},  // last wins: always on
}

// TestGCGrammarGoMatchesSQL guards against drift between queues.GCActivation
// (Go) and entroq.gc_activation (SQL): for every vector, malformed-ness (Go err
// vs SQL NULL) and due-ness (Go !at.After(now) vs SQL activate_at <= now) must
// agree with each other and with the expected result.
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
		var sqlAt sql.NullTime
		if err := b.DB.QueryRowContext(ctx, "SELECT entroq.gc_activation($1)", v.queue).Scan(&sqlAt); err != nil {
			t.Fatalf("gc_activation(%q): %v", v.queue, err)
		}
		sqlMalformed := !sqlAt.Valid
		sqlDue := sqlAt.Valid && !sqlAt.Time.After(now)

		goMalformed, goDue := goActivation(v.queue, now)

		if goMalformed != v.malformed || goDue != v.due {
			t.Errorf("%q: Go malformed=%v due=%v, want malformed=%v due=%v", v.queue, goMalformed, goDue, v.malformed, v.due)
		}
		if sqlMalformed != v.malformed || sqlDue != v.due {
			t.Errorf("%q: SQL malformed=%v due=%v, want malformed=%v due=%v", v.queue, sqlMalformed, sqlDue, v.malformed, v.due)
		}
		if goMalformed != sqlMalformed || goDue != sqlDue {
			t.Errorf("%q: Go/SQL DISAGREE (go m=%v d=%v, sql m=%v d=%v) -- grammar drift",
				v.queue, goMalformed, goDue, sqlMalformed, sqlDue)
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

// TestGCCollectOnce drives the discover-then-collect path directly (white-box) to
// pin the SQL semantics: gc_queues finds the gc= queues, and gc_collect (fed the
// valid ones) reaps due gc= tasks while leaving not-yet-due activation, future
// arrival (claimed-equivalent), and non-gc queues alone. The GC interval is set
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

	// Discover, relaying only the valid (non-malformed) queues to collectOnce; the
	// SQL still filters future activations and future task arrivals.
	qs, err := b.gcQueues(ctx)
	if err != nil {
		t.Fatalf("gcQueues: %v", err)
	}
	var queues []string
	var activations []time.Time
	for _, q := range qs {
		if q.activateAt.Valid {
			queues = append(queues, q.queue)
			activations = append(activations, q.activateAt.Time)
		}
	}

	n, err := b.collectOnce(ctx, queues, activations, 100)
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
