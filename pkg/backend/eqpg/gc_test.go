package eqpg

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
)

var retiredSQLProcedures = []string{
	"entroq.modify(text,jsonb,jsonb,jsonb,jsonb)",
	"entroq.modify_docs(text,jsonb,jsonb,jsonb,jsonb)",
	"entroq.queues(text,text[],integer)",
	"entroq.tasks(text,integer,boolean)",
	"entroq.docs(text,text,text,integer,boolean)",
	"entroq.claim_docs(text,text,interval,text)",
	"entroq.like_prefix(text)",
	"entroq.gc_collect(text[],timestamptz[],integer)",
	"entroq.gc_queues()",
	"entroq.gc_activation(text)",
	"entroq._path_param_values(text,text)",
	"entroq.gc_due(text)",
}

var retiredSQLTypes = []string{
	"entroq.task_arg",
	"entroq.doc_arg",
	"entroq.task_id",
	"entroq.doc_id",
}

var retiredSQLIndexes = []string{
	"entroq.bygcqueueat",
	"entroq.bycompoundgcqueueat",
}

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

func TestLegacySQLSurfaceRemoved(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	b, err := Open(ctx, pgHostPort,
		WithDB("postgres"), WithUsername("postgres"), WithPassword("password"),
		WithConnectAttempts(10), withGCInterval(time.Hour))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer b.Close()
	assertLegacySQLSurfaceRemoved(ctx, t, b.DB)
}

func assertLegacySQLSurfaceRemoved(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	for _, signature := range retiredSQLProcedures {
		var present bool
		if err := db.QueryRowContext(ctx, "SELECT to_regprocedure($1) IS NOT NULL", signature).Scan(&present); err != nil {
			t.Fatalf("look up procedure %q: %v", signature, err)
		}
		if present {
			t.Errorf("retired GC procedure %q is still installed", signature)
		}
	}

	for _, name := range retiredSQLTypes {
		var present bool
		if err := db.QueryRowContext(ctx, "SELECT to_regtype($1) IS NOT NULL", name).Scan(&present); err != nil {
			t.Fatalf("look up type %q: %v", name, err)
		}
		if present {
			t.Errorf("retired raw-SQL type %q is still installed", name)
		}
	}

	for _, name := range retiredSQLIndexes {
		var present bool
		if err := db.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&present); err != nil {
			t.Fatalf("look up index %q: %v", name, err)
		}
		if present {
			t.Errorf("retired GC index %q is still installed", name)
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

func TestGCDocGroups(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := Open(ctx, pgHostPort,
		WithDB("postgres"), WithUsername("postgres"), WithPassword("password"),
		WithConnectAttempts(10), withGCInterval(time.Hour))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer b.Close()
	eqtest.GCDocGroups(ctx, t, b, b.collectDocsOnce, "/pgtest/"+entroq.GenHex16())
}

// TestGCCollectOnce drives the shared claim/delete collector directly: due gc=
// tasks are reaped while future activation, future arrival, and plain queues
// survive. The GC interval is held off so the loop cannot race the assertions.
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

	p := "/test/collectonce/" + entroq.GenHex16()
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
	// Task IDs are unique across queues; give this run's cases their own.
	for i := range cases {
		cases[i].id += "-" + entroq.GenHex16()
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
	if n < 2 {
		t.Errorf("collectOnce deleted %d, want at least 2 (co_due0, co_past)", n)
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

// TestCollectLocksSkipsGroupBeingInserted holds an insert into an empty,
// unheld group open while lock collection runs. The insert leaves the lock
// row unchanged, so only its shared lock on the row keeps collection from
// deleting the lock of a group that is about to have a member.
func TestCollectLocksSkipsGroupBeingInserted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := Open(ctx, pgHostPort,
		WithDB("postgres"), WithUsername("postgres"), WithPassword("password"),
		WithConnectAttempts(10), withGCInterval(time.Hour))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer b.Close()

	ns := "/pgtest/" + entroq.GenHex16()
	lockExists := func() bool {
		t.Helper()
		var n int
		if err := b.DB.QueryRowContext(ctx, `SELECT count(*) FROM entroq.doc_locks WHERE namespace = $1 AND key_primary = 'k'`, ns).Scan(&n); err != nil {
			t.Fatalf("Read lock: %v", err)
		}
		return n == 1
	}

	// An empty group whose claim has lapsed: idle, so collectable.
	if _, err := b.ClaimDocs(ctx, &entroq.DocClaim{Namespace: ns, Key: "k", Claimant: "me", Duration: time.Millisecond}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	tx, err := b.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	mod := entroq.NewModification("me", entroq.PuttingDocInto(ns, entroq.WithKeys("k", "a")))
	if err := modifyDocs(ctx, tx, mod, new(entroq.ModifyResponse)); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Collection may wait for the insert, as a one-statement delete does, so
	// run it alongside and commit the insert while it is going. A delete that
	// waits then rechecks only the lock row, which the insert did not change,
	// and removes the lock of a group that now has a member.
	collected := make(chan error, 1)
	go func() {
		_, err := b.collectLocksOnce(ctx, 1000)
		collected <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit insert: %v", err)
	}
	if err := <-collected; err != nil {
		t.Fatalf("Collect during insert: %v", err)
	}
	if !lockExists() {
		t.Fatal("Collection deleted the lock of a group with an insert in progress")
	}
	if _, err := b.collectLocksOnce(ctx, 1000); err != nil {
		t.Fatalf("Collect after insert: %v", err)
	}
	if !lockExists() {
		t.Error("Collection deleted the lock of a group with a member")
	}
}
