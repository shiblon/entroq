package eqpg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
	"github.com/shiblon/entroq/pkg/worker"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	_ "github.com/lib/pq"
)

var pgHostPort string

// dockerAvailable reports whether a Docker daemon is reachable. The eqpg tests
// run Postgres in a testcontainer, so without Docker there is nothing to test
// against. Detecting its absence lets TestMain skip cleanly (exit 0) instead of
// failing the package -- important because a bare `go test ./...` (or any CI
// without a Docker service) would otherwise hard-fail here, and the Example_*
// functions, which run unconditionally, would crash on an empty endpoint.
func dockerAvailable(ctx context.Context) bool {
	cli, err := testcontainers.NewDockerClient()
	if err != nil {
		return false
	}
	defer cli.Close()
	// NewDockerClient does not fail when the daemon is unreachable (it swallows
	// the probe and hands back an env-derived client), so the ping is the real
	// check. Bound it: an unreachable or black-hole endpoint must not hang here.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = cli.Ping(ctx)
	return err == nil
}

func TestMain(m *testing.M) {
	ctx := context.Background()

	if !dockerAvailable(ctx) {
		log.Println("SKIP: Docker is not available; skipping eqpg integration tests (they require a Postgres testcontainer).")
		os.Exit(0)
	}

	ctr, err := postgres.Run(ctx, "postgres:17",
		postgres.WithPassword("password"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		log.Fatalf("Postgres start: %v", err)
	}
	defer func() {
		if err := ctr.Terminate(ctx); err != nil {
			log.Printf("Postgres stop: %v", err)
		}
	}()

	pgHostPort, err = ctr.Endpoint(ctx, "")
	if err != nil {
		log.Fatalf("Postgres endpoint: %v", err)
	}

	backend, err := Open(ctx, pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(10),
		WithInitSchema())
	if err != nil {
		log.Fatalf("Postgres open: %v", err)
	}
	backend.Close()

	os.Exit(m.Run())
}

// TestReadinessLoop checks that a claim blocked on a future task is woken by
// the backend's readiness loop, well before the 30-second claim poll.
func TestReadinessLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := pgClient(ctx)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer client.Close()

	queue := fmt.Sprintf("/test/readiness/%d", time.Now().UnixNano())

	// Arrive in the future so the insert itself wakes no one.
	at := time.Now().Add(2 * time.Second)
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithArrivalTime(at))); err != nil {
		t.Fatalf("Failed to insert delayed task: %v", err)
	}

	// The default readiness interval is 5s, so the claim should return within
	// about 7s; the poll would take 30s.
	claimCtx, claimCancel := context.WithTimeout(ctx, 10*time.Second)
	defer claimCancel()

	task, err := client.Claim(claimCtx, entroq.From(queue), entroq.ClaimFor(time.Second))
	if err != nil {
		t.Fatalf("Failed to claim task: %v", err)
	}

	t.Logf("Successfully claimed delayed task: %v", task.ID)
}

func TestReadinessFanout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const interval = 500 * time.Millisecond
	client, err := entroq.New(ctx, Opener(pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(10),
		WithReadinessInterval(interval)))
	if err != nil {
		t.Fatalf("Open client: %v", err)
	}
	defer client.Close()
	eqtest.ReadinessFanout(interval)(ctx, t, client, fmt.Sprintf("/test/fanout/%d", time.Now().UnixNano()))
}

// TestUpgradeDropsRetiredReadiness recreates the LISTEN/NOTIFY readiness
// objects a 1.11.0 schema has, then reapplies the schema and checks that each
// one is gone.
func TestUpgradeDropsRetiredReadiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	backend, err := Open(ctx, pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(10))
	if err != nil {
		t.Fatalf("Open backend: %v", err)
	}
	defer backend.Close()
	db := backend.DB

	for _, stmt := range []string{
		"CREATE INDEX IF NOT EXISTS byAt ON entroq.tasks (at, queue)",
		"CREATE TABLE IF NOT EXISTS entroq.notification_state (id INTEGER PRIMARY KEY CHECK (id = 1), last_at TIMESTAMPTZ NOT NULL)",
		"CREATE OR REPLACE FUNCTION entroq.channel_name(p_queue text) RETURNS text LANGUAGE sql IMMUTABLE STRICT AS $$ SELECT 'q_' || p_queue $$",
		"CREATE OR REPLACE FUNCTION entroq.notify_ready_queues(p_min_interval interval DEFAULT '0 seconds') RETURNS SETOF text LANGUAGE sql AS $$ SELECT ''::text WHERE false $$",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("Recreate retired object: %v\n%s", err, stmt)
		}
	}

	if err := InitSchema(ctx, db); err != nil {
		t.Fatalf("Reapply schema: %v", err)
	}

	checks := []struct {
		name  string
		query string
	}{
		{"byAt index", "SELECT count(*) FROM pg_indexes WHERE schemaname = 'entroq' AND indexname = 'byat'"},
		{"notification_state table", "SELECT count(*) FROM pg_tables WHERE schemaname = 'entroq' AND tablename = 'notification_state'"},
		{"channel_name function", "SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'entroq' AND p.proname = 'channel_name'"},
		{"notify_ready_queues function", "SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'entroq' AND p.proname = 'notify_ready_queues'"},
	}
	for _, c := range checks {
		var n int
		if err := db.QueryRowContext(ctx, c.query).Scan(&n); err != nil {
			t.Fatalf("Check %s: %v", c.name, err)
		}
		if n != 0 {
			t.Errorf("%s still present after schema upgrade", c.name)
		}
	}
}

// freshDB creates an empty database in the shared PostgreSQL instance and
// drops it when the test ends.
func freshDB(ctx context.Context, t *testing.T, name string) *sql.DB {
	t.Helper()
	admin, err := OpenDB(pgHostPort, WithDB("postgres"), WithUsername("postgres"), WithPassword("password"))
	if err != nil {
		t.Fatalf("Open admin db: %v", err)
	}
	t.Cleanup(func() { admin.Close() })
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name); err != nil {
		t.Fatalf("Drop stale database: %v", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("Create database: %v", err)
	}
	db, err := OpenDB(pgHostPort, WithDB(name), WithUsername("postgres"), WithPassword("password"))
	if err != nil {
		t.Fatalf("Open fresh db: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		if _, err := admin.ExecContext(context.Background(), "DROP DATABASE "+name); err != nil {
			t.Logf("Drop test database (non-fatal): %v", err)
		}
	})
	return db
}

// TestUpgradeFrom1_11 upgrades a database initialized with the last released
// schema (1.11.0, shipped through 1.12.x), whose docs carried their own
// versions and claims. Each group must get a lock one version past its
// highest member, with claims released; the per-doc columns must be gone and
// every doc tied to its group's lock; and the digest must let the backend
// open, and stop it opening once it no longer matches.
func TestUpgradeFrom1_11(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const name = "entroq_upgrade_from_1_11"
	db := freshDB(ctx, t, name)

	old, err := os.ReadFile("testdata/schema-1.11.0.sql")
	if err != nil {
		t.Fatalf("Read 1.11.0 schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(old)); err != nil {
		t.Fatalf("Apply 1.11.0 schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO entroq.docs (namespace, id, version, claimant, at, key_primary, key_secondary, value) VALUES
		('ns', 'a', 2, '', now(), 'k', '1', '"a"'),
		('ns', 'b', 7, 'holder', now() + interval '1 hour', 'k', '2', '"b"'),
		('ns', 'c', 0, '', now(), 'other', '', '"c"')`); err != nil {
		t.Fatalf("Insert 1.11.0 docs: %v", err)
	}

	for range 2 { // the second pass must change nothing
		if _, err := UpgradeSchema(ctx, db); err != nil {
			t.Fatalf("Upgrade: %v", err)
		}
	}

	var dropped int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_attribute
		WHERE attrelid = 'entroq.docs'::regclass AND attname IN ('version', 'claimant', 'at') AND NOT attisdropped`).Scan(&dropped); err != nil {
		t.Fatalf("Read docs columns: %v", err)
	}
	if dropped != 0 {
		t.Errorf("Docs keep %d of their per-doc version, claimant, and at columns", dropped)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO entroq.docs (namespace, id, key_primary) VALUES ('ns', 'orphan', 'nolock')`); err == nil {
		t.Error("Inserted a doc whose group has no lock")
	}

	b, err := Open(ctx, pgHostPort, WithDB(name), WithUsername("postgres"), WithPassword("password"), WithConnectAttempts(10))
	if err != nil {
		t.Fatalf("Open upgraded database: %v", err)
	}
	defer b.Close()
	docs, err := b.Docs(ctx, &entroq.DocQuery{Namespace: "ns"})
	if err != nil || len(docs) != 3 {
		t.Fatalf("Docs after upgrade: %v, %v", docs, err)
	}
	want := map[string]int32{"a": 8, "b": 8, "c": 1}
	for _, d := range docs {
		if d.Version != want[d.ID] || d.Claimant != "" {
			t.Errorf("Upgraded doc %q: want version %d and no claimant, got version %d, claimant %q", d.ID, want[d.ID], d.Version, d.Claimant)
		}
	}
	if _, err := b.Modify(ctx, entroq.NewModification("me", docs[0].Change(entroq.WithContent("after")))); err != nil {
		t.Errorf("Change after upgrade: %v", err)
	}

	// Another build's schema at the same version, or one applied without a
	// digest, must not open.
	for _, stmt := range []string{
		`UPDATE entroq.meta SET value = 'other' WHERE key = 'schema_digest'`,
		`DELETE FROM entroq.meta WHERE key = 'schema_digest'`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("Change digest: %v", err)
		}
		if bad, err := Open(ctx, pgHostPort, WithDB(name), WithUsername("postgres"), WithPassword("password"), WithConnectAttempts(1)); err == nil {
			bad.Close()
			t.Errorf("Opened a database after %q", stmt)
		} else if !strings.Contains(err.Error(), "schema upgrade") {
			t.Errorf("Open after %q: want advice to run schema upgrade, got %v", stmt, err)
		}
		if res, err := UpgradeSchema(ctx, db); err != nil || res != UpgradeApplied {
			t.Fatalf("Upgrade after %q: %v, %v", stmt, res, err)
		}
	}
}

func pgClient(ctx context.Context) (client *entroq.EntroQ, err error) {
	return entroq.New(ctx, Opener(pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(10)))
}

func Example() {
	ctx := context.Background()
	client, err := entroq.New(ctx, Opener( // eqpg.Opener
		pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(2)))
	if err != nil {
		log.Fatalf("Entroq init error: %v", err)
	}
	defer client.Close()

	// Insert some tasks.
	_, err = client.Modify(ctx,
		entroq.InsertingInto("/example/queue 1", entroq.WithValue("hello")),
		entroq.InsertingInto("/example/queue 2", entroq.WithValue("hello")),
	)
	if err != nil {
		log.Fatalf("insertion failed: %v", err)
	}

	// For the sake of the example: cancel the worker after 2 seconds. Usually you won't ever cancel a worker.
	// Note that timeouts are considered unclean shutdowns, so we do a pure cancel in a goroutine.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { time.Sleep(2 * time.Second); cancel() }()

	w := worker.New(client,
		worker.WithDoWork(func(ctx context.Context, claimed *entroq.Task, s string, _ []*entroq.Doc) error {
			// Do work with the task.
			fmt.Println(s)
			return nil
		}),
		worker.WithFinish(func(ctx context.Context, mod worker.Modifier, final *entroq.Task, _ string, _ []*entroq.Doc) error {
			// Delete the task to "commit" the work.
			// At this point, you can also call directly into eqpg.ModifyOpts and
			// hand it a function to call that has a transaction. That transaction
			// is what Modify uses to commit task changes, and you can do other
			// database operations in it for fully atomic commits. This is a good
			// pattern for updating state data while handling tasks, to ensure that
			// it all happens at once.
			if _, err := mod.Modify(ctx, final.Delete()); err != nil {
				return fmt.Errorf("Failed to delete/commit task: %w", err)
			}
			return nil
		}),
	)
	if err := w.Run(ctx, worker.Watching("/example/queue 1", "/example/queue 2")); err != nil {
		log.Fatal(err)
	}

	// Output:
	// hello
	// hello
}

func Example_inTransaction() {
	ctx := context.Background()
	// Create the backend separately to get access to the database for raw SQL.
	backend, err := Open(ctx, // eqpg.Open
		pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(2))
	if err != nil {
		log.Fatalf("EQPG failed to open: %v", err)
	}
	client, err := entroq.New(ctx, nil /* no opener */, entroq.WithBackend(backend))
	if err != nil {
		log.Fatalf("Entroq init error: %v", err)
	}
	defer client.Close()

	// Prep a simple database table. We'll update it inside the Modify transaction.
	if _, err := backend.DB.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS exampleCounter (id int);
		 TRUNCATE exampleCounter;
		 INSERT INTO exampleCounter (id) VALUES (0);`,
	); err != nil {
		log.Fatalf("Failed to prep exampleCounter: %v", err)
	}

	// Insert some tasks.
	_, err = client.Modify(ctx,
		entroq.InsertingInto("/example/queue 1", entroq.WithValue("hello")),
		entroq.InsertingInto("/example/queue 2", entroq.WithValue("hello")),
		entroq.InsertingInto("/example/queue 2", entroq.WithValue("hello")),
	)
	if err != nil {
		log.Fatalf("insertion failed: %v", err)
	}

	// For the sake of the example: cancel the worker after 2 seconds. Usually you won't ever cancel a worker.
	// Note that timeouts are considered unclean shutdowns, so we do a pure cancel in a goroutine.
	workerCtx, cancel := context.WithCancel(ctx) // don't overwrite the main context, we'll need it after Run!
	defer cancel()
	go func() { time.Sleep(2 * time.Second); cancel() }()

	// Create a worker that just prints the task value, then in finalization,
	// when the task version is finalized (background renewal is stopped),
	// updates the counter table.
	w := worker.New(client,
		worker.WithDoWork(func(ctx context.Context, claimed *entroq.Task, s string, _ []*entroq.Doc) error {
			// Do work with the task.
			fmt.Println(s)
			return nil
		}),
		worker.WithFinish(func(ctx context.Context, mod worker.Modifier, final *entroq.Task, _ string, _ []*entroq.Doc) error {
			// Delete the task to "commit" the work.

			// The counter is updated in the same transaction as the entroq modification.
			// If either fails, the entire transaction fails, leaving all work
			// items in a consistent state.
			inTx := func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, "UPDATE exampleCounter SET id = id + 1")
				return err
			}
			if _, err := mod.Modify(ctx, final.Delete(), entroq.WithModifyOption(RunningInTx(inTx))); err != nil {
				return fmt.Errorf("Failed to delete/commit task: %w", err)
			}
			return nil
		}),
	)
	if err := w.Run(workerCtx, worker.Watching("/example/queue 1", "/example/queue 2")); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("worker run failed: %v", err)
	}

	// The worker's context is canceled, so we use a fresh context to check the DB.
	var count int
	if err := backend.DB.QueryRowContext(ctx, "SELECT id FROM exampleCounter").Scan(&count); err != nil {
		log.Fatalf("Failed to get count: %v", err)
	}
	fmt.Printf("Count: %d\n", count)

	// Output:
	// hello
	// hello
	// hello
	// Count: 3
}

func RunQTest(t *testing.T, tester eqtest.Tester) {
	t.Helper()
	ctx := context.Background()
	client, err := pgClient(ctx)
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer client.Close()
	tester(ctx, t, client, "pgtest/"+client.GenID())
}

func TestTasksWithID(t *testing.T) {
	RunQTest(t, eqtest.TasksWithID)
}

func TestTasksOmitValue(t *testing.T) {
	RunQTest(t, eqtest.TasksOmitValue)
}

func TestTasksWithIDOnly(t *testing.T) {
	RunQTest(t, eqtest.TasksWithIDOnly)
}

func TestInsertWithID(t *testing.T) {
	RunQTest(t, eqtest.InsertWithID)
}

func TestSimpleSequence(t *testing.T) {
	RunQTest(t, eqtest.SimpleSequence)
}

func TestSimpleChange(t *testing.T) {
	RunQTest(t, eqtest.SimpleChange)
}

func TestChangeKeepsStoredFields(t *testing.T) {
	RunQTest(t, eqtest.ChangeKeepsStoredFields)
}

func TestInsertKeepsAttemptAndErr(t *testing.T) {
	RunQTest(t, eqtest.InsertKeepsAttemptAndErr)
}

func TestModifyRejectsDuplicateIDs(t *testing.T) {
	RunQTest(t, eqtest.ModifyRejectsDuplicateIDs)
}

func TestModifyRespectsTaskClaims(t *testing.T) {
	RunQTest(t, eqtest.ModifyRespectsTaskClaims)
}

func TestTasksWithIDStaysInQueue(t *testing.T) {
	RunQTest(t, eqtest.TasksWithIDStaysInQueue)
}

func TestDocGroups(t *testing.T) {
	RunQTest(t, eqtest.DocGroups)
}

func TestTaskChangeFarPastArrivalNormalized(t *testing.T) {
	RunQTest(t, eqtest.TaskChangeFarPastArrivalNormalized)
}

func TestModifyRejectsWrongQueue(t *testing.T) {
	RunQTest(t, eqtest.ModifyRejectsWrongQueue)
}

func TestEmptyWriteTargetRejected(t *testing.T) {
	RunQTest(t, eqtest.EmptyWriteTargetRejected)
}

func TestSimpleWorker(t *testing.T) {
	RunQTest(t, eqtest.SimpleWorker)
}

func TestMultiWorker(t *testing.T) {
	RunQTest(t, eqtest.MultiWorker)
}

func TestWorkerMoveOnError(t *testing.T) {
	RunQTest(t, eqtest.WorkerMoveOnError)
}

func TestWorkerRetryOnError(t *testing.T) {
	RunQTest(t, eqtest.WorkerRetryOnError)
}

func TestClaimUnblocksOnNotify(t *testing.T) {
	RunQTest(t, eqtest.ClaimUnblocksOnNotify)
}

func TestQueueMatch(t *testing.T) {
	RunQTest(t, eqtest.QueueMatch)
}

func TestQueuePrefixMatchLiteral(t *testing.T) {
	RunQTest(t, eqtest.QueuePrefixMatchLiteral)
}

func TestPGNamespacePrefixMatchLiteral(t *testing.T) {
	RunQTest(t, eqtest.NamespacePrefixMatchLiteral)
}

func TestQueueStats(t *testing.T) {
	RunQTest(t, eqtest.QueueStats)
}

func TestQueueStatsLimit(t *testing.T) {
	RunQTest(t, eqtest.QueueStatsLimit)
}

func TestDeleteMissingTask(t *testing.T) {
	RunQTest(t, eqtest.DeleteMissingTask)
}

func TestClaimRandomHead(t *testing.T) {
	RunQTest(t, eqtest.ClaimRandomHead)
}

func TestTasksClaimantLimit(t *testing.T) {
	RunQTest(t, eqtest.TasksClaimantLimit)
}

func TestLengthLimits(t *testing.T) {
	RunQTest(t, eqtest.LengthLimits)
}

func TestClaimLongDuration(t *testing.T) {
	RunQTest(t, eqtest.ClaimLongDuration)
}

func TestMapReduce(t *testing.T) {
	RunQTest(t, eqtest.MapReduce)
}

func Example_disableListenNotify() {
	// 1. Connection poolers in "transaction mode" (like PgBouncer) break the Postgres
	//    LISTEN/NOTIFY strategy because they re-assign the underlying server connection
	//    to a different client as soon as a transaction finishes.
	// 2. EntroQ gracefully handles dropped notifications by eventually falling back to
	//    a standard polling interval. However, allowing the broken LISTEN attempts to
	//    run will unnecessarily hold open dedicated pool connections, potentially
	//    starving your pool.
	// 3. To efficiently run behind a transaction mode pooler, you can disable the
	//    notification strategy entirely by setting the NotifyWaiter to nil.
	//
	// Note: there are no really good ways to get cross-client notifications for queue
	// updates when using a connection pooling proxy with postgres. The good news is,
	// if you need a connection pool in the first, place, it's likely you're
	// trying to run with many workers (i.e., more than 50), and if you don't
	// have a ton of queues that they're working on, the default polling
	// interval (which only applies to each individual worker) won't be noticed
	// as much. If you have 30 workers on a single queue, a 30-second polling
	// interval feels like a 1-second interval on average.
	//
	// All that said, if you really want reliable cross-client notifications,
	// you can also use a service like the provided gRPC service that connects
	// to postgres and exposes gRPC and JSON endpoints. Connect to that and
	// notifications work fine.
	ctx := context.Background()
	backend, err := Open(ctx,
		pgHostPort,
		WithConnectAttempts(2),
		WithNotifyWaiter(nil), // disables notifications
	)
	if err != nil {
		log.Fatalf("pg open failed: %v", err)
	}
	defer backend.Close()

	// Then, in your worker, rely entirely on polling instead of waiting for pushes:
	// client.Claim(ctx, entroq.From("/example/my_queue"), entroq.ClaimPollTime(5 * time.Second))
	fmt.Println("notifications disabled")

	// Output:
	// notifications disabled
}

func TestWorkerCompactDependencyHandler(t *testing.T) {
	RunQTest(t, eqtest.WorkerCompactDependencyHandler)
}

func TestWorkerDependencyMove(t *testing.T) {
	RunQTest(t, eqtest.WorkerDependencyMove)
}

func TestWorkerHoldsEmptyGroup(t *testing.T) {
	RunQTest(t, eqtest.WorkerHoldsEmptyGroup)
}

func TestPGSimpleDocLifecycle(t *testing.T) {
	RunQTest(t, eqtest.SimpleDocLifecycle)
}

func TestPGInitialVersions(t *testing.T) {
	RunQTest(t, eqtest.InitialVersions)
}

func TestPGDocMultiOp(t *testing.T) {
	RunQTest(t, eqtest.DocMultiOp)
}

func TestPGDocTimestamps(t *testing.T) {
	RunQTest(t, eqtest.DocTimestamps)
}

func TestPGDocListing(t *testing.T) {
	RunQTest(t, eqtest.DocListing)
}

func TestPGDocKeyRangeByteOrder(t *testing.T) {
	RunQTest(t, eqtest.DocKeyRangeByteOrder)
}

func TestPGDocClaimLocking(t *testing.T) {
	RunQTest(t, eqtest.DocClaimLocking)
}

func TestPGDocInsertWithID(t *testing.T) {
	RunQTest(t, eqtest.DocInsertWithID)
}

func TestPGDocClaimantBehavior(t *testing.T) {
	RunQTest(t, eqtest.DocClaimantBehavior)
}

func TestPGQueueStatsAccuracy(t *testing.T) {
	RunQTest(t, eqtest.QueueStatsAccuracy)
}

func TestPGNamespaceStats(t *testing.T) {
	RunQTest(t, eqtest.NamespaceStats)
}

// TestSchemaInit verifies that InitSchema succeeds on a blank database,
// records the correct SchemaVersion, and is idempotent when run a second time.
// Uses a fresh database within the shared testcontainers postgres instance so
// that it exercises the exact path that TestMain bypasses (TestMain already has
// the schema applied when m.Run() fires).
func TestSchemaInit(t *testing.T) {
	const testDB = "entroq_schema_init_test"
	ctx := context.Background()

	adminDB, err := OpenDB(pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"))
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	defer adminDB.Close()

	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+testDB); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	freshDB, err := OpenDB(pgHostPort,
		WithDB(testDB),
		WithUsername("postgres"),
		WithPassword("password"))
	if err != nil {
		t.Fatalf("open fresh db: %v", err)
	}

	// Ping to confirm the connection is live before we try anything.
	if err := freshDB.PingContext(ctx); err != nil {
		freshDB.Close()
		t.Fatalf("ping fresh db: %v", err)
	}

	// First apply: blank database.
	if err := InitSchema(ctx, freshDB); err != nil {
		freshDB.Close()
		t.Fatalf("InitSchema on blank db: %v", err)
	}

	got, err := StoredSchemaVersion(ctx, freshDB)
	if err != nil {
		freshDB.Close()
		t.Fatalf("StoredSchemaVersion after init: %v", err)
	}
	if got != SchemaVersion {
		freshDB.Close()
		t.Errorf("StoredSchemaVersion after init: want %q, got %q", SchemaVersion, got)
	}

	// Simulate an upgrade from a schema that still exposed the raw-SQL surface.
	// The dummy bodies are enough to recreate every retired identity so the next
	// schema application proves its DROP statements actually converge an
	// existing database, rather than merely succeeding when those objects never
	// existed.
	if _, err := freshDB.ExecContext(ctx, `
		CREATE TYPE entroq.task_arg AS (value text);
		CREATE TYPE entroq.doc_arg AS (value text);
		CREATE TYPE entroq.task_id AS (value text);
		CREATE TYPE entroq.doc_id AS (value text);
		CREATE FUNCTION entroq.modify(text,jsonb,jsonb,jsonb,jsonb) RETURNS integer LANGUAGE sql AS 'SELECT 1';
		CREATE FUNCTION entroq.modify_docs(text,jsonb,jsonb,jsonb,jsonb) RETURNS integer LANGUAGE sql AS 'SELECT 1';
		CREATE FUNCTION entroq.queues(text,text[],integer) RETURNS integer LANGUAGE sql AS 'SELECT 1';
		CREATE FUNCTION entroq.tasks(text,integer,boolean) RETURNS integer LANGUAGE sql AS 'SELECT 1';
		CREATE FUNCTION entroq.docs(text,text,text,integer,boolean) RETURNS integer LANGUAGE sql AS 'SELECT 1';
		CREATE FUNCTION entroq.claim_docs(text,text,interval,text) RETURNS integer LANGUAGE sql AS 'SELECT 1';
		CREATE FUNCTION entroq.like_prefix(text) RETURNS integer LANGUAGE sql AS 'SELECT 1';
		CREATE FUNCTION entroq.gc_collect(text[],timestamptz[],integer) RETURNS integer LANGUAGE sql AS 'SELECT 1';
		CREATE FUNCTION entroq.gc_queues() RETURNS integer LANGUAGE sql AS 'SELECT 1';
		CREATE FUNCTION entroq.gc_activation(text) RETURNS integer LANGUAGE sql AS 'SELECT 1';
		CREATE FUNCTION entroq._path_param_values(text,text) RETURNS integer LANGUAGE sql AS 'SELECT 1';
		CREATE FUNCTION entroq.gc_due(text) RETURNS integer LANGUAGE sql AS 'SELECT 1';
		CREATE INDEX byGCQueueAt ON entroq.tasks (queue, at) WHERE queue LIKE '%/gc=%';
		CREATE INDEX byCompoundGCQueueAt ON entroq.tasks (queue, at) WHERE queue LIKE '%;gc=%';
	`); err != nil {
		freshDB.Close()
		t.Fatalf("seed retired schema surface: %v", err)
	}

	// Second apply: idempotency plus legacy-object convergence.
	if err := InitSchema(ctx, freshDB); err != nil {
		freshDB.Close()
		t.Fatalf("InitSchema idempotency: %v", err)
	}

	got, err = StoredSchemaVersion(ctx, freshDB)
	if err != nil {
		freshDB.Close()
		t.Fatalf("StoredSchemaVersion after re-init: %v", err)
	}
	if got != SchemaVersion {
		freshDB.Close()
		t.Errorf("StoredSchemaVersion after re-init: want %q, got %q", SchemaVersion, got)
	}
	assertLegacySQLSurfaceRemoved(ctx, t, freshDB)

	// Close the fresh connection before dropping -- postgres refuses to drop a
	// database with open connections.
	freshDB.Close()

	if _, err := adminDB.ExecContext(ctx, "DROP DATABASE "+testDB); err != nil {
		t.Logf("drop test database (non-fatal): %v", err)
	}
}

func TestModifyReportsAllFailureClasses(t *testing.T) {
	RunQTest(t, eqtest.ModifyReportsAllFailureClasses)
}

func TestModifyRejectsWrongNamespace(t *testing.T) {
	RunQTest(t, eqtest.ModifyRejectsWrongNamespace)
}

// TestRunningInTxCommitsWithModify checks that caller work done through
// RunningInTx commits or rolls back with the modification, including when the
// modification fails in Go rather than in the database, which leaves the
// transaction healthy enough to commit.
func TestRunningInTxCommitsWithModify(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := Open(ctx, pgHostPort, WithDB("postgres"), WithUsername("postgres"), WithPassword("password"), WithConnectAttempts(10))
	if err != nil {
		t.Fatalf("Open backend: %v", err)
	}
	defer b.Close()
	if _, err := b.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS public.running_in_tx_test (id text PRIMARY KEY)`); err != nil {
		t.Fatalf("Create table: %v", err)
	}

	prefix := entroq.GenHex16()
	queue := "/test/running-in-tx/" + prefix
	written := func(id string) bool {
		t.Helper()
		var n int
		if err := b.DB.QueryRowContext(ctx, `SELECT count(*) FROM public.running_in_tx_test WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("Read caller row: %v", err)
		}
		return n == 1
	}
	modify := func(id string, fail error, args ...entroq.ModifyArg) error {
		args = append(args, entroq.WithModifyOption(RunningInTx(func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `INSERT INTO public.running_in_tx_test (id) VALUES ($1)`, id); err != nil {
				return err
			}
			return fail
		})))
		_, err := b.Modify(ctx, entroq.NewModification("me", args...))
		return err
	}

	if err := modify(prefix+"-fails", errors.New("caller failed"), entroq.InsertingInto(queue)); err == nil {
		t.Error("Modify with failing caller work: want an error")
	}
	if written(prefix + "-fails") {
		t.Error("Caller work committed although the caller failed")
	}

	missing := entroq.NewDocID(queue, "missing", 0)
	if err := modify(prefix+"-dep", nil, entroq.InsertingInto(queue), missing.Depend()); !entroq.IsDependency(err) {
		t.Errorf("Modify depending on a missing doc: want a dependency error, got %v", err)
	}
	if written(prefix + "-dep") {
		t.Error("Caller work committed although the modification failed a doc dependency")
	}

	if err := modify(prefix+"-ok", nil, entroq.InsertingInto(queue)); err != nil {
		t.Fatalf("Modify: %v", err)
	}
	if !written(prefix + "-ok") {
		t.Error("Caller work did not commit with a successful modification")
	}
}

func TestInvalidRequests(t *testing.T) {
	RunQTest(t, eqtest.InvalidRequests)
}

func TestBackendRejectsInvalidRequests(t *testing.T) {
	ctx := context.Background()
	b, err := Open(ctx, pgHostPort, WithDB("postgres"), WithUsername("postgres"), WithPassword("password"), WithConnectAttempts(10))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer b.Close()
	eqtest.BackendRejectsInvalidRequests(ctx, t, b, "/pgtest/"+entroq.GenHex16())
	eqtest.StorageRejectsZeroDurations(ctx, t, b, "/pgtest/"+entroq.GenHex16())
}

func TestTasksClaimantFilter(t *testing.T) {
	RunQTest(t, eqtest.TasksClaimantFilter)
}

func TestQueueStatsCounts(t *testing.T) {
	RunQTest(t, eqtest.QueueStatsCounts)
}

func TestQueueStatsMatching(t *testing.T) {
	RunQTest(t, eqtest.QueueStatsMatching)
}

func TestDocsOrderAndLimits(t *testing.T) {
	RunQTest(t, eqtest.DocsOrderAndLimits)
}

func TestTaskClaimantIsHolder(t *testing.T) {
	RunQTest(t, eqtest.TaskClaimantIsHolder)
}

func TestTaskChangeFutureArrival(t *testing.T) {
	RunQTest(t, eqtest.TaskChangeFutureArrival)
}

// TestDocConcurrencyStress is slow, so it runs in parallel with the other parallel tests
// once the sequential ones finish; it uses its own namespace and queue.
func TestDocConcurrencyStress(t *testing.T) {
	t.Parallel()
	RunQTest(t, eqtest.DocConcurrencyStress)
}

// TestMixedAtomicStress is slow, so it runs in parallel with the other parallel tests
// once the sequential ones finish; it uses its own namespace and queue.
func TestMixedAtomicStress(t *testing.T) {
	t.Parallel()
	RunQTest(t, eqtest.MixedAtomicStress)
}

// TestCanceledQueryIsCanceled cancels a modification while its transaction is
// running a query. lib/pq then reports the server's "canceling statement due
// to user request", which must still read as the caller's cancellation.
func TestCanceledQueryIsCanceled(t *testing.T) {
	ctx := context.Background()
	b, err := Open(ctx, pgHostPort, WithDB("postgres"), WithUsername("postgres"), WithPassword("password"), WithConnectAttempts(10))
	if err != nil {
		t.Fatalf("Open backend: %v", err)
	}
	defer b.Close()

	cctx, cancel := context.WithCancel(ctx)
	running := make(chan struct{})
	go func() {
		<-running
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err = b.Modify(cctx, entroq.NewModification("me",
		entroq.InsertingInto("/test/canceled-query"),
		entroq.WithModifyOption(RunningInTx(func(ctx context.Context, tx *sql.Tx) error {
			close(running)
			_, err := tx.ExecContext(ctx, "SELECT pg_sleep(10)")
			return err
		})),
	))
	if !entroq.IsCanceled(err) {
		t.Errorf("Modify canceled mid-query: want a cancellation, got %v", err)
	}
}
