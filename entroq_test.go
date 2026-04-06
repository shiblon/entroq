package entroq_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/backend/eqmem"
)

func Example() {
	// Create an in-memory EntroQ instance.
	// Other backends are available, notably eqpg to use Postgres, or eqgrpc to
	// speak to EntroQ as a GRPC client. In the GRPC case, see cmd/eq*svc to
	// start up the server side of a GRPC connection.
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())

	// Queues appear and disappear as tasks are added or removed. They are
	// essentially an ephemeral concept. Names are just text.
	// Task values are []byte and can be basically anything, but size matters:
	// files are best represented as paths, for example.

	// Insert a few tasks:
	ins, _, err := eq.Modify(ctx,
		entroq.InsertingInto("q1", entroq.WithValue([]byte("hello 1"))),
		entroq.InsertingInto("q2", entroq.WithValue([]byte("hello 2"))))
	if err != nil {
		log.Fatalf("Error inserting tasks: %v", err)
	}

	// You can get auto-assigned IDs, versions, etc. from each inserted task:
	for _, t := range ins {
		log.Printf("Task inserted: %s\n", t)
	}

	// Fire up a worker to read them. Workers can listen on one or more queues
	// simultaneously. If more than one queue is specified, it will attempt to
	// read from them uniformly randomly for fairness. This can be used to
	// "wedge in" tasks that need to be handled quickly while larger batches are
	// going elsewhere.
	worker := eq.NewWorker(entroq.FuncHandler(
		// Workers claim a task and pass it to your handler functions. In the
		// background, the task's lease is renewed while the first function runs.
		func(ctx context.Context, initial *entroq.Task) error {
			log.Printf("Worker handling task %s", initial)
			// Do work with it here.
			return nil
		},
		// When ready to commit changes to the task (including deletion), the second
		// function passes the version-stable task after the renewer is stopped,
		// making it safe to use it in modification transactions.
		func(ctx context.Context, final *entroq.Task) error {
			log.Printf("Deleting task %s", final)
			_, _, err := eq.Modify(ctx, final.Delete())
			if err != nil {
				return err
			}
			return nil
		},
	))

	// The worker runs forever, so for the sake of this example we pass it a context
	// and cancel it. Other errors typicall just cause a retry (including timeouts).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { time.Sleep(2 * time.Second); cancel() }()

	log.Fatal(worker.Run(ctx, "q1", "q2"))
}

func Example_dependencies() {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("failed to open memory backend: %v", err)
	}

	// 1. Insert a configuration task and a worker task.
	_, _, err = eq.Modify(ctx,
		entroq.InsertingInto("config", entroq.WithValue([]byte(`{"max_retries": 5}`))),
		entroq.InsertingInto("worker", entroq.WithValue([]byte(`do work`))),
	)
	if err != nil {
		log.Fatalf("insert failed: %v", err)
	}

	// 2. We use a worker that depends on the config task. If the config is modified
	// while the worker is processing, the worker's commit will fail, and it will retry.
	var config *entroq.Task

	worker := eq.NewWorker(entroq.FuncHandler(
		func(ctx context.Context, initial *entroq.Task) error {
			if config == nil {
				tasks, err := eq.Tasks(ctx, "config")
				if err != nil || len(tasks) == 0 {
					return fmt.Errorf("failed to load config")
				}
				config = tasks[0]
			}
			// ... do work with initial and config ...
			return nil
		},
		func(ctx context.Context, final *entroq.Task) error {
			if config == nil {
				return fmt.Errorf("config missing during finalize")
			}

			_, _, err := eq.Modify(ctx, final.Delete(), config.Depend())
			if err != nil {
				if depErr, ok := entroq.AsDependency(err); ok {
					// Check if our config task is the one that caused the dependency collision.
					// If so, we clear it and return a retry error so the worker loop
					// starts over and fetches the new config.
					for _, depTask := range depErr.Depends {
						if depTask.ID == config.ID {
							config = nil
							return entroq.RetryTaskErrorf("config changed during work, retrying")
						}
					}
				}
				return fmt.Errorf("commit failed: %w", err)
			}
			return nil
		},
	))

	// For the sake of the example: cancel the worker after 2 seconds.
	// You won't actually do this in production.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { time.Sleep(2 * time.Second); cancel() }()

	if err := worker.Run(ctx, "worker"); err != nil && !entroq.IsCanceled(err) {
		log.Fatalf("worker failed: %v", err)
	}
}

func Example_manualClaimAndRenew() {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatal(err)
	}

	// While eq.NewWorker is recommended for general use, you can manually
	// claim and renew tasks for custom daemon implementations.

	// Insert a task to work on.
	if _, _, err := eq.Modify(ctx, entroq.InsertingInto("manual_queue", entroq.WithValue([]byte("work")))); err != nil {
		log.Fatalf("insert failed: %v", err)
	}

	// 1. Manually claim the task.
	task, err := eq.Claim(ctx, entroq.From("manual_queue"), entroq.ClaimFor(5*time.Second))
	if err != nil {
		log.Fatalf("claim failed: %v", err)
	}

	// 2. Wrap your work in DoWithRenew so the task doesn't expire while you work.
	err = eq.DoWithRenew(ctx, task, 5*time.Second, func(ctx context.Context, stop entroq.FinalizeRenew) error {

		// ... do some long running work ...

		// 3. Stop background renewal to get a stable, finalized version of the task.
		finalTask := stop()

		// 4. Commit the work (by deleting the task or mutating it).
		if _, _, err := eq.Modify(ctx, finalTask.Delete()); err != nil {
			return fmt.Errorf("failed to commit: %w", err)
		}
		return nil
	})

	if err != nil {
		log.Fatalf("manual processing failed: %v", err)
	}
}
