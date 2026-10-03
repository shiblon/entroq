package eqsqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
)

func TestBackendContract(t *testing.T) {
	tests := []struct {
		name string
		run  eqtest.Tester
	}{
		{"TasksWithID", eqtest.TasksWithID},
		{"TasksOmitValue", eqtest.TasksOmitValue},
		{"TasksWithIDOnly", eqtest.TasksWithIDOnly},
		{"InsertWithID", eqtest.InsertWithID},
		{"SimpleSequence", eqtest.SimpleSequence},
		{"SimpleChange", eqtest.SimpleChange},
		{"ChangeKeepsStoredFields", eqtest.ChangeKeepsStoredFields},
		{"InsertKeepsAttemptAndErr", eqtest.InsertKeepsAttemptAndErr},
		{"ModifyRejectsDuplicateIDs", eqtest.ModifyRejectsDuplicateIDs},
		{"ModifyRespectsTaskClaims", eqtest.ModifyRespectsTaskClaims},
		{"TasksWithIDStaysInQueue", eqtest.TasksWithIDStaysInQueue},
		{"DocSets", eqtest.DocSets},
		{"UpdateArrival", eqtest.UpdateArrival},
		{"ClaimStampsLease", eqtest.ClaimStampsLease},
		{"TaskChangeFutureArrival", eqtest.TaskChangeFutureArrival},
		{"TaskChangeFarPastArrivalNormalized", eqtest.TaskChangeFarPastArrivalNormalized},
		{"ModifyRejectsWrongQueue", eqtest.ModifyRejectsWrongQueue},
		{"EmptyWriteTargetRejected", eqtest.EmptyWriteTargetRejected},
		{"SimpleWorker", eqtest.SimpleWorker},
		{"MultiWorker", eqtest.MultiWorker},
		{"WorkerMoveOnError", eqtest.WorkerMoveOnError},
		{"WorkerRetryOnError", eqtest.WorkerRetryOnError},
		{"ClaimUnblocksOnNotify", eqtest.ClaimUnblocksOnNotify},
		{"QueueMatch", eqtest.QueueMatch},
		{"QueuePrefixMatchLiteral", eqtest.QueuePrefixMatchLiteral},
		{"NamespacePrefixMatchLiteral", eqtest.NamespacePrefixMatchLiteral},
		{"QueueStats", eqtest.QueueStats},
		{"QueueStatsLimit", eqtest.QueueStatsLimit},
		{"DeleteMissingTask", eqtest.DeleteMissingTask},
		{"ClaimRandomHead", eqtest.ClaimRandomHead},
		{"TasksClaimantLimit", eqtest.TasksClaimantLimit},
		{"LengthLimits", eqtest.LengthLimits},
		{"ClaimLongDuration", eqtest.ClaimLongDuration},
		{"MapReduce", eqtest.MapReduce},
		{"WorkerCompactDependencyHandler", eqtest.WorkerCompactDependencyHandler},
		{"WorkerDependencyMove", eqtest.WorkerDependencyMove},
		{"WorkerHoldsEmptyGroup", eqtest.WorkerHoldsEmptyGroup},
		{"WorkerReleasesSets", eqtest.WorkerReleasesSets},
		{"ClaimsReset", eqtest.ClaimsReset},
		{"DocClaimSets", eqtest.DocClaimSets},
		{"InvalidRequests", eqtest.InvalidRequests},
		{"TasksClaimantFilter", eqtest.TasksClaimantFilter},
		{"QueueStatsCounts", eqtest.QueueStatsCounts},
		{"QueueStatsMatching", eqtest.QueueStatsMatching},
		{"DocsOrderAndLimits", eqtest.DocsOrderAndLimits},
		{"TaskClaimantIsHolder", eqtest.TaskClaimantIsHolder},
		{"InitialVersions", eqtest.InitialVersions},
		{"SimpleDocLifecycle", eqtest.SimpleDocLifecycle},
		{"DocMultiOp", eqtest.DocMultiOp},
		{"DocTimestamps", eqtest.DocTimestamps},
		{"DocListing", eqtest.DocListing},
		{"DocKeyRangeByteOrder", eqtest.DocKeyRangeByteOrder},
		{"DocClaimLocking", eqtest.DocClaimLocking},
		{"DocInsertWithID", eqtest.DocInsertWithID},
		{"DocClaimantBehavior", eqtest.DocClaimantBehavior},
		{"DocConcurrencyStress", eqtest.DocConcurrencyStress},
		{"MixedAtomicStress", eqtest.MixedAtomicStress},
		{"QueueStatsAccuracy", eqtest.QueueStatsAccuracy},
		{"NamespaceStats", eqtest.NamespaceStats},
		{"ModifyReportsAllFailureClasses", eqtest.ModifyReportsAllFailureClasses},
		{"ModifyRejectsWrongNamespace", eqtest.ModifyRejectsWrongNamespace},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			client, err := entroq.New(ctx, Opener(filepath.Join(t.TempDir(), "entroq.sqlite")))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer client.Close()
			test.run(ctx, t, client, "sqlitetest")
		})
	}
}

func TestReadinessFanout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const interval = 500 * time.Millisecond
	client, err := entroq.New(ctx, Opener(filepath.Join(t.TempDir(), "entroq.sqlite"), WithReadinessInterval(interval)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer client.Close()
	eqtest.ReadinessFanout(interval)(ctx, t, client, "sqlitetest")
}

func TestPersistenceAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "entroq.sqlite")
	client, err := entroq.New(ctx, Opener(path))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Modify(ctx,
		entroq.InsertingInto("persistent/tasks", entroq.WithID("task-1"), entroq.WithValue("task")),
		entroq.PuttingDocInto("persistent/docs", entroq.WithIDKeys("doc-1", "key", "secondary"), entroq.WithContent("doc")),
	)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := client.TryClaim(ctx, entroq.From("persistent/tasks"), entroq.ClaimFor(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	client, err = entroq.New(ctx, Opener(path))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	tasks, err := client.Tasks(ctx, "persistent/tasks")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks after reopen: len=%d err=%v", len(tasks), err)
	}
	if tasks[0].Version != claimed.Version || tasks[0].Claims != 1 || tasks[0].ID != resp.InsertedTasks[0].ID {
		t.Fatalf("task state after reopen: %#v", tasks[0])
	}
	docs, err := client.Docs(ctx, &entroq.DocQuery{Namespace: "persistent/docs"})
	if err != nil || len(docs) != 1 || docs[0].ID != resp.InsertedDocs[0].ID {
		t.Fatalf("docs after reopen: docs=%v err=%v", docs, err)
	}
}

func TestTasksByIDWithoutQueuePreserveRequestOrder(t *testing.T) {
	ctx := context.Background()
	client, err := entroq.New(ctx, Opener(filepath.Join(t.TempDir(), "entroq.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Modify(ctx,
		entroq.InsertingInto("q1", entroq.WithID("z")),
		entroq.InsertingInto("q2", entroq.WithID("a")),
	); err != nil {
		t.Fatal(err)
	}
	tasks, err := client.Tasks(ctx, "", entroq.WithTaskID("z", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].ID != "z" || tasks[1].ID != "a" {
		t.Fatalf("tasks by ID: got %v, want [z a]", tasks)
	}
}

func TestRejectsUnknownSchemaVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "entroq.sqlite")
	b, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", sqliteDSN(path, 5*time.Second, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE entroq_meta SET schema_version = 999 WHERE id = 1"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path); err == nil {
		t.Fatal("opened database with an unknown schema version")
	}
}

func TestConcurrentClaimsAreUnique(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := entroq.New(ctx, Opener(filepath.Join(t.TempDir(), "entroq.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	const count = 100
	args := make([]entroq.ModifyArg, 0, count)
	for i := range count {
		args = append(args, entroq.InsertingInto("contended", entroq.WithID(fmt.Sprintf("task-%03d", i))))
	}
	if _, err := client.Modify(ctx, args...); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	ids := make(chan string, count)
	for worker := range 16 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for {
				task, err := client.TryClaim(ctx, entroq.From("contended"),
					entroq.WithClaimant(fmt.Sprintf("worker-%d", worker)), entroq.ClaimFor(time.Hour))
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if task == nil {
					return
				}
				ids <- task.ID
			}
		}(worker)
	}
	wg.Wait()
	close(ids)
	seen := make(map[string]bool, count)
	for id := range ids {
		if seen[id] {
			t.Fatalf("task claimed twice: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != count {
		t.Fatalf("claimed %d tasks, want %d", len(seen), count)
	}
}

func TestConcurrentBackendsShareWriterLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "entroq.sqlite")
	const backendCount = 4
	clients := make([]*entroq.EntroQ, 0, backendCount)
	for range backendCount {
		client, err := entroq.New(ctx, Opener(path))
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		defer client.Close()
	}

	const count = 100
	args := make([]entroq.ModifyArg, 0, count)
	for i := range count {
		args = append(args, entroq.InsertingInto("shared", entroq.WithID(fmt.Sprintf("task-%03d", i))))
	}
	if _, err := clients[0].Modify(ctx, args...); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	ids := make(chan string, count)
	for worker := range 16 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			client := clients[worker%len(clients)]
			for {
				task, err := client.TryClaim(ctx, entroq.From("shared"),
					entroq.WithClaimant(fmt.Sprintf("worker-%d", worker)), entroq.ClaimFor(time.Hour))
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if task == nil {
					return
				}
				ids <- task.ID
			}
		}(worker)
	}
	wg.Wait()
	close(ids)
	seen := make(map[string]bool, count)
	for id := range ids {
		if seen[id] {
			t.Fatalf("task claimed twice across backend handles: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != count {
		t.Fatalf("claimed %d tasks, want %d", len(seen), count)
	}
}

func TestBlockingClaimAfterInitialMissDoesNotWaitForWriter(t *testing.T) {
	ctx := context.Background()
	b, err := Open(ctx, filepath.Join(t.TempDir(), "entroq.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// Claim makes one optimistic write attempt so a ready backlog does not pay
	// for a separate readiness query. Once that misses, empty-queue polling must
	// stay on the read/subq path rather than repeatedly entering the write pool.
	claimCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := b.Claim(claimCtx, &entroq.ClaimQuery{
			Queues:   []string{"empty"},
			Claimant: "waiter",
			Duration: time.Minute,
		})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)

	tx, err := b.writeDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	before := b.writeDB.Stats().WaitCount
	time.Sleep(100 * time.Millisecond)
	if got := b.writeDB.Stats().WaitCount; got != before {
		t.Fatalf("blocking claim retried through writer after initial miss: WaitCount changed from %d to %d", before, got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking claim did not stop after cancellation")
	}
}

func TestGCLoopCollects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := entroq.New(ctx, Opener(filepath.Join(t.TempDir(), "entroq.sqlite"), withGCInterval(10*time.Millisecond)))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	eqtest.GCCollectsInLoop(ctx, t, client, "sqlitetest")
}

func TestGCDocSets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	b, err := Open(ctx, filepath.Join(t.TempDir(), "entroq.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	eqtest.GCDocSets(ctx, t, b, b.collectDocsOnce, "sqlitetest")
}

func TestBackendRejectsInvalidRequests(t *testing.T) {
	ctx := context.Background()
	b, err := Open(ctx, filepath.Join(t.TempDir(), "entroq.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	eqtest.BackendRejectsInvalidRequests(ctx, t, b, "sqlitetest")
	eqtest.StorageRejectsZeroDurations(ctx, t, b, "sqlitetest")
}

// TestWriteEndedByContextIsCanceled checks that a write whose context ends
// mid-transaction reports the cancellation. database/sql rolls such a
// transaction back itself, and Commit then says only that it is done, which a
// worker would take for a failure rather than a clean stop.
func TestWriteEndedByContextIsCanceled(t *testing.T) {
	b, err := Open(context.Background(), filepath.Join(t.TempDir(), "entroq.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	_, err = b.write(ctx, func(_ context.Context, tx *sql.Tx) (any, error) {
		cancel()
		// Wait for database/sql to roll the transaction back.
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
			var one int
			if err := tx.QueryRowContext(context.Background(), "SELECT 1").Scan(&one); errors.Is(err, sql.ErrTxDone) {
				return nil, nil
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("transaction not rolled back after cancel")
			}
		}
	})
	if !entroq.IsCanceled(err) {
		t.Errorf("Write ended by its context: want a cancellation, got %v", err)
	}
}
