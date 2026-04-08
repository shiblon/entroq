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
	"github.com/shiblon/entroq/pkg/testing/eqtest"
	"github.com/shiblon/entroq/pkg/worker"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	_ "github.com/lib/pq"
)

var pgHostPort string

func TestMain(m *testing.M) {
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, "postgres:11",
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

// TestNotifyReadyQueues directly invokes notify_ready_queues() and asserts
// that it returns the expected queue when a task's arrival time has passed.
// This pins the watermark logic in SQL independently of any polling fallback.
func TestNotifyReadyQueues(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	client, err := entroq.New(ctx, nil, entroq.WithBackend(backend))
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	queue := fmt.Sprintf("/test/notify-ready/%d", time.Now().UnixNano())
	if _, _, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithArrivalTimeIn(500*time.Millisecond))); err != nil {
		t.Fatalf("Insert delayed task: %v", err)
	}

	// Wait for the task to become ready.
	time.Sleep(600 * time.Millisecond)

	// Directly call notify_ready_queues with no minimum interval.
	rows, err := backend.DB.QueryContext(ctx, "SELECT entroq.notify_ready_queues('0 seconds'::interval)")
	if err != nil {
		t.Fatalf("notify_ready_queues: %v", err)
	}
	defer rows.Close()

	var notified []string
	for rows.Next() {
		var q string
		if err := rows.Scan(&q); err != nil {
			t.Fatalf("scan: %v", err)
		}
		notified = append(notified, q)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	for _, q := range notified {
		if q == queue {
			return
		}
	}
	t.Fatalf("notify_ready_queues did not return %q; got %v", queue, notified)
}

func TestReadinessTicker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := pgClient(ctx)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer client.Close()

	queue := fmt.Sprintf("/test/readiness/%d", time.Now().UnixNano())

	// Insert a task that becomes ready in 2 seconds.
	// The immediate trigger won't fire for this one.
	at := time.Now().Add(2 * time.Second)
	if _, _, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithArrivalTime(at))); err != nil {
		t.Fatalf("Failed to insert delayed task: %v", err)
	}

	// Claim should block until notified or timeout.
	// We expect the ticker to fire around the 5s mark.
	claimCtx, claimCancel := context.WithTimeout(ctx, 10*time.Second)
	defer claimCancel()

	task, err := client.Claim(claimCtx, entroq.From(queue), entroq.ClaimFor(time.Second))
	if err != nil {
		t.Fatalf("Failed to claim task: %v", err)
	}

	t.Logf("Successfully claimed delayed task: %v", task.ID)
}

func TestReadinessTicker_LocalOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Create a client with Local-Only notifications. No LISTEN/NOTIFY.
	client, err := entroq.New(ctx, Opener(pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(10),
		WithHeartbeat(5*time.Second),
		WithNoListen()))
	if err != nil {
		t.Fatalf("Failed to create local-only client: %v", err)
	}
	defer client.Close()

	queue := fmt.Sprintf("/test/readiness-local/%d", time.Now().UnixNano())

	// Insert a delayed task.
	if _, _, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithArrivalTimeIn(2*time.Second))); err != nil {
		t.Fatalf("Failed to insert delayed task: %v", err)
	}

	// Claim should block until notified by the ticker (since no PG notify is being listened to).
	claimCtx, claimCancel := context.WithTimeout(ctx, 10*time.Second)
	defer claimCancel()

	task, err := client.Claim(claimCtx, entroq.From(queue), entroq.ClaimFor(time.Second))
	if err != nil {
		t.Fatalf("Failed to claim task: %v", err)
	}

	t.Logf("Successfully claimed delayed task: %v", task.ID)
}

func pgClient(ctx context.Context) (client *entroq.EntroQ, err error) {
	return entroq.New(ctx, Opener(pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(10),
		WithHeartbeat(5*time.Second)))
}

func Example() {
	ctx := context.Background()
	client, err := entroq.New(ctx, Opener( // eqpg.Opener
		pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(2),
		WithHeartbeat(5*time.Second)))
	if err != nil {
		log.Fatalf("Entroq init error: %v", err)
	}
	defer client.Close()

	// Insert some tasks.
	_, _, err = client.Modify(ctx,
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
		worker.WithDo(func(ctx context.Context, claimed *entroq.Task, s string) error {
			// Do work with the task.
			fmt.Println(s)
			return nil
		}),
		worker.WithFinish(func(ctx context.Context, final *entroq.Task, _ string) error {
			// Delete the task to "commit" the work.
			// At this point, you can also call directly into eqpg.ModifyOpts and
			// hand it a function to call that has a transaction. That transaction
			// is what Modify uses to commit task changes, and you can do other
			// database operations in it for fully atomic commits. This is a good
			// pattern for updating state data while handling tasks, to ensure that
			// it all happens at once.
			if _, _, err := client.Modify(ctx, final.Delete()); err != nil {
				return fmt.Errorf("Failed to delete/commit task: %w", err)
			}
			return nil
		}),
	)
	if err := w.Run(ctx, "/example/queue 1", "/example/queue 2"); err != nil {
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
	_, _, err = client.Modify(ctx,
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
		worker.WithDo(func(ctx context.Context, claimed *entroq.Task, s string) error {
			// Do work with the task.
			fmt.Println(s)
			return nil
		}),
		worker.WithFinish(func(ctx context.Context, final *entroq.Task, _ string) error {
			// Delete the task to "commit" the work.

			// The counter is updated in the same transaction as the entroq modification.
			// If either fails, the entire transaction fails, leaving all work
			// items in a consistent state.
			inTx := func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, "UPDATE exampleCounter SET id = id + 1")
				return err
			}
			if _, _, err := client.Modify(ctx, final.Delete(), entroq.WithModifyOption(RunningInTx(inTx))); err != nil {
				return fmt.Errorf("Failed to delete/commit task: %w", err)
			}
			return nil
		}),
	)
	if err := w.Run(workerCtx, "/example/queue 1", "/example/queue 2"); err != nil && !errors.Is(err, context.Canceled) {
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

func TestWorkerRenewal(t *testing.T) {
	RunQTest(t, eqtest.WorkerRenewal)
}

func TestClaimUnblocksOnNotify(t *testing.T) {
	RunQTest(t, eqtest.ClaimUnblocksOnNotify)
}

func TestQueueMatch(t *testing.T) {
	RunQTest(t, eqtest.QueueMatch)
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
}
