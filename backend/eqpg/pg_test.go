package eqpg

import (
	"context"
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
	// pgHostPort was set outside of this function.

	ctx := context.Background()

	// Open the backend separately so we have access to special functions that
	// are postgres-specific.
	backend, err := Open(ctx, pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(10))
	if err != nil {
		log.Fatalf("Backend error: %v", err)
	}

	client, err := entroq.New(ctx, nil, entroq.WithBackend(backend))
	if err != nil {
		log.Fatalf("Entroq init error: %v", err)
	}

	// Insert some tasks.
	_, _, err = client.Modify(ctx,
		entroq.InsertingInto("queue 1", entroq.WithValue([]byte("hello"))),
		entroq.InsertingInto("queue 2", entroq.WithValue([]byte("hello"))),
	)
	if err != nil {
		log.Fatalf("insertion failed: %v", err)
	}

	// For the sake of the example: cancel the worker after 2 seconds. Usually you won't ever cancel a worker.
	// Note that timeouts are considered unclean shutdowns, so we do a pure cancel in a goroutine.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		time.Sleep(2 * time.Second)
		cancel()
	}()

	// Start a worker, process the tasks.
	worker := NewPGWorker(client, "queue 1", "queue 2")
	if err := worker.Run(ctx, func(ctx context.Context, task *entroq.Task, finalize entroq.FinalizeRenew) error {
		// Do the work
		fmt.Println(string(task.Value))

		// Stop renewal, get final task version.
		renewed := finalize()

		// Delete the task to "commit" the work.
		// At this point, you can also call directly into eqpg.ModifyOpts and
		// hand it a function to call that has a transaction. That transaction
		// is what Modify uses to commit task changes, and you can do other
		// database operations in it for fully atomic commits. This is a good
		// pattern for updating state data while handling tasks, to ensure that
		// it all happens at once.
		if _, _, err := client.Modify(ctx, renewed.AsDeletion()); err != nil {
			return fmt.Errorf("Failed to delete/commit task: %w", err)
		}
		return nil
	}); err != nil {
		log.Fatal(err)
	}

	// Output:
	// hello
	// hello
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
