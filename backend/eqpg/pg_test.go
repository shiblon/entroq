package eqpg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/qsvc/qtest"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	_ "github.com/lib/pq"
)

var pgHostPort string

func TestMain(m *testing.M) {
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx, "postgres:11",
		tcpostgres.WithPassword("password"),
		tcpostgres.BasicWaitStrategies(),
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

	// Insert some tasks.
	_, _, err = client.Modify(ctx,
		entroq.InsertingInto("/example/queue 1", entroq.WithValue([]byte("hello"))),
		entroq.InsertingInto("/example/queue 2", entroq.WithValue([]byte("hello"))),
	)
	if err != nil {
		log.Fatalf("insertion failed: %v", err)
	}

	// For the sake of the example: cancel the worker after 2 seconds. Usually you won't ever cancel a worker.
	// Note that timeouts are considered unclean shutdowns, so we do a pure cancel in a goroutine.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { time.Sleep(2 * time.Second); cancel() }()

	worker := entroq.NewWorker(client, entroq.FuncHandler(
		func(ctx context.Context, claimed *entroq.Task) error {
			// Do work with the task.
			fmt.Println(string(claimed.Value))
			return nil
		},
		func(ctx context.Context, final *entroq.Task) error {
			// Delete the task to "commit" the work.
			// At this point, you can also call directly into eqpg.ModifyOpts and
			// hand it a function to call that has a transaction. That transaction
			// is what Modify uses to commit task changes, and you can do other
			// database operations in it for fully atomic commits. This is a good
			// pattern for updating state data while handling tasks, to ensure that
			// it all happens at once.
			if _, _, err := client.Modify(ctx, final.AsDeletion()); err != nil {
				return fmt.Errorf("Failed to delete/commit task: %w", err)
			}
			return nil
		},
	))
	if err := worker.Run(ctx, "/example/queue 1", "/example/queue 2"); err != nil {
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

	// Prep a simple database table. We'll update it inside the Modify transaction.
	if _, err := backend.DB.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS exampleCounter (id int);
		 TRUNCATE exampleCounter;
		 INSERT INTO exampleCounter (id) VALUES (0);`,
	); err != nil {
		log.Fatalf("Failed to prep exampleCounter: %v", err)
	}

	// Insert some tasks.
	_, _, err = client.Modify(ctx,
		entroq.InsertingInto("/example/queue 1", entroq.WithValue([]byte("hello"))),
		entroq.InsertingInto("/example/queue 2", entroq.WithValue([]byte("hello"))),
		entroq.InsertingInto("/example/queue 2", entroq.WithValue([]byte("hello"))),
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
	worker := entroq.NewWorker(client, entroq.FuncHandler(
		func(ctx context.Context, claimed *entroq.Task) error {
			// Do work with the task.
			fmt.Println(string(claimed.Value))
			return nil
		},
		func(ctx context.Context, final *entroq.Task) error {
			// Delete the task to "commit" the work.

			// The counter is updated in the same transaction as the entroq modification.
			// If either fails, the entire transaction fails, leaving all work
			// items in a consistent state.
			inTx := func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, "UPDATE exampleCounter SET id = id + 1")
				return err
			}
			if _, _, err := client.Modify(ctx,
				final.AsDeletion(),
				entroq.WithModifyOption(RunningInTx(inTx)),
			); err != nil {
				return fmt.Errorf("Failed to delete/commit task: %w", err)
			}
			return nil
		},
	))
	if err := worker.Run(workerCtx, "/example/queue 1", "/example/queue 2"); err != nil && !errors.Is(err, context.Canceled) {
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

func RunQTest(t *testing.T, tester qtest.Tester) {
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
	RunQTest(t, qtest.TasksWithID)
}

func TestTasksOmitValue(t *testing.T) {
	RunQTest(t, qtest.TasksOmitValue)
}

func TestTasksWithIDOnly(t *testing.T) {
	RunQTest(t, qtest.TasksWithIDOnly)
}

func TestInsertWithID(t *testing.T) {
	RunQTest(t, qtest.InsertWithID)
}

func TestSimpleSequence(t *testing.T) {
	RunQTest(t, qtest.SimpleSequence)
}

func TestSimpleChange(t *testing.T) {
	RunQTest(t, qtest.SimpleChange)
}

func TestSimpleWorker(t *testing.T) {
	RunQTest(t, qtest.SimpleWorker)
}

func TestMultiWorker(t *testing.T) {
	RunQTest(t, qtest.MultiWorker)
}

func TestWorkerMoveOnError(t *testing.T) {
	RunQTest(t, qtest.WorkerMoveOnError)
}

func TestWorkerRetryOnError(t *testing.T) {
	RunQTest(t, qtest.WorkerRetryOnError)
}

func TestWorkerRenewal(t *testing.T) {
	RunQTest(t, qtest.WorkerRenewal)
}

func TestClaimUnblocksOnNotify(t *testing.T) {
	RunQTest(t, qtest.ClaimUnblocksOnNotify)
}

func TestQueueMatch(t *testing.T) {
	RunQTest(t, qtest.QueueMatch)
}

func TestQueueStats(t *testing.T) {
	RunQTest(t, qtest.QueueStats)
}

func TestDeleteMissingTask(t *testing.T) {
	RunQTest(t, qtest.DeleteMissingTask)
}

func Example_disableListenNotify() {
	// 1. Connection poolers in "transaction mode" (like PgBouncer) break the Postgres
	//    LISTEN/NOTIFY strategy because they re-assign the underlying server connection
	//    to a different client as soon as a transaction finishes.
	// 2. EntroQ gracefully handles dropped notifications by eventually falling back to
	//    a standard polling interval. However, allowing the broken LISTEN attempts to
	//    run will unnecessarily hold open dedicated pool connections, potentially
	//    starving your pool.
	// 3. To safely run behind a transaction mode pooler, you should disable the
	//    notification strategy entirely by setting the NotifyWaiter to nil.
	//
	// You must disable the NotifyWaiter when opening the backend (you can also
	// use eqpg.Opener in entroq.New if you don't need eqpg-specific options):
	ctx := context.Background()
	backend, err := Open(ctx,
		pgHostPort,
		WithConnectAttempts(2),
		WithNotifyWaiter(nil), // <--- Critical for PgBouncer Transaction Mode!
	)
	if err != nil {
		log.Fatalf("pg open failed: %v", err)
	}
	defer backend.Close()

	// Then, in your worker, rely entirely on polling instead of waiting for pushes:
	// client.Claim(ctx, entroq.From("/example/my_queue"), entroq.ClaimPollTime(5 * time.Second))
}


