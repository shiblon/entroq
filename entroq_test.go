package entroq_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/worker"
)

func Example() {
	// Create an in-memory EntroQ instance.
	// Other backends are available, notably eqpg to use Postgres, or eqgrpc to
	// speak to EntroQ as a GRPC client. In the GRPC case, see cmd/eq*svc to
	// start up the server side of a GRPC connection.
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("Can't open eq: %v", err)
	}
	defer eq.Close()

	// Queues appear and disappear as tasks are added or removed. They are
	// essentially an ephemeral concept. Names are just text.
	// Task values are JSON and can be any valid JSON value.

	// Insert a few tasks:
	resp, err := eq.Modify(ctx,
		entroq.InsertingInto("q1", entroq.WithValue("hello")),
		entroq.InsertingInto("q2", entroq.WithValue("hello")))
	if err != nil {
		log.Fatalf("Error inserting tasks: %v", err)
	}

	// You can get auto-assigned IDs, versions, etc. from each inserted task:
	for _, t := range resp.InsertedTasks {
		log.Printf("Task inserted: %s\n", t.ID)
	}

	// Fire up a worker to read them. Workers can listen on one or more queues
	// simultaneously. If more than one queue is specified, it will attempt to
	// read from them uniformly randomly for fairness. This can be used to
	// "wedge in" tasks that need to be handled quickly while larger batches are
	// going elsewhere.
	w := worker.New(eq,
		// Workers claim a task and pass it to your handler functions. In the
		// background, the task's lease is renewed while the first function runs.
		worker.WithDo(func(ctx context.Context, initial *entroq.Task, v string) error {
			fmt.Printf("Worker handling task %q\n", v)
			// Do work with it here.
			return nil
		}),
		// When ready to commit changes to the task (including deletion), the second
		// function passes the version-stable task after the renewer is stopped,
		// making it safe to use it in modification transactions.
		worker.WithFinish(func(ctx context.Context, final *entroq.Task, v string) error {
			fmt.Printf("Deleting task %q\n", v)
			_, err := eq.Modify(ctx, final.Delete())
			if err != nil {
				return err
			}
			return nil
		}),
	)

	// The worker runs forever, so for the sake of this example we cancel it
	// pretty quickly. Other errors typically just cause a retry.
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	if err := w.Run(ctx, worker.Watching("q1", "q2")); err != nil {
		log.Fatal(err)
	}

	// Output:
	// Worker handling task "hello"
	// Deleting task "hello"
	// Worker handling task "hello"
	// Deleting task "hello"
}

func Example_dependencies() {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("failed to open memory backend: %v", err)
	}
	defer eq.Close()

	// 1. Insert a configuration task and a worker task.
	_, err = eq.Modify(ctx,
		entroq.InsertingInto("config", entroq.WithRawValue(json.RawMessage(`{"max_retries": 5}`))), // []byte() also works
		entroq.InsertingInto("worker", entroq.WithValue("do work")),
	)
	if err != nil {
		log.Fatalf("insert failed: %v", err)
	}

	// 2. We use a worker that depends on the config task. If the config is modified
	// while the worker is processing, the worker's commit will fail, and it will retry.
	var config *entroq.Task

	w := worker.New(eq,
		worker.WithDo(func(ctx context.Context, initial *entroq.Task, _ json.RawMessage) error {
			if config == nil {
				tasks, err := eq.Tasks(ctx, "config")
				if err != nil || len(tasks) == 0 {
					return fmt.Errorf("failed to load config")
				}
				config = tasks[0]
			}
			// ... do work with initial and config ...
			return nil
		}),
		worker.WithFinish(func(ctx context.Context, final *entroq.Task, _ json.RawMessage) error {
			if config == nil {
				return fmt.Errorf("config missing during finalize")
			}

			_, err := eq.Modify(ctx, final.Delete(), config.Depend())
			if err != nil {
				if depErr, ok := entroq.AsDependency(err); ok {
					if len(depErr.Depends) > 0 {
						// It was our config task that caused the failed commit.
						// Something changed it out from under us, so we can't
						// trust that the work we did was done with the right
						// config. We could just fall off the end and the task will
						// be picked up again after its arrival time expires.
						// Or we could force a retry and increment it's attempts
						// (return RetryError). But there's nothing wrong with the task
						// so far as we know, so make it immediately available.
						if _, err := eq.Modify(ctx, final.Change(entroq.ArrivalTimeBy(0))); err != nil {
							// NOW it's our task that's the problem. Just bail.
							return fmt.Errorf("task reset after config change: %w", err)
						}
					}
				}
				return fmt.Errorf("commit failed: %w", err)
			}
			return nil
		}),
	)

	// For the sake of the example: cancel the worker after 2 seconds.
	// You won't actually do this in production.
	ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	if err := w.Run(ctx, worker.Watching("worker")); err != nil && !entroq.IsCanceled(err) {
		log.Fatalf("worker failed: %v", err)
	}
}

func Example_manualClaimAndRenew() {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatal(err)
	}
	defer eq.Close()

	// While eq.NewWorker is recommended for general use, you can manually
	// claim and renew tasks for custom daemon implementations.

	// Insert a task to work on.
	if _, err := eq.Modify(ctx, entroq.InsertingInto("manual_queue", entroq.WithValue("work"))); err != nil {
		log.Fatalf("insert failed: %v", err)
	}

	// 1. Manually claim the task.
	task, err := eq.Claim(ctx, entroq.From("manual_queue"), entroq.ClaimFor(5*time.Second))
	if err != nil {
		log.Fatalf("claim failed: %v", err)
	}

	// 2. Wrap your work in worker.DoWithRenew so the task doesn't expire while you work.
	err = worker.DoWithRenew(ctx, eq, task, 5*time.Second, func(ctx context.Context, stop worker.FinalizeRenew) error {

		// ... do some long running work ...

		// 3. Stop background renewal to get a stable, finalized version of the task.
		finalTask := stop()

		// 4. Commit the work (by deleting the task or mutating it).
		if _, err := eq.Modify(ctx, finalTask.Delete()); err != nil {
			return fmt.Errorf("failed to commit: %w", err)
		}
		return nil
	})

	if err != nil {
		log.Fatalf("manual processing failed: %v", err)
	}
}

func TestDoWithRenewAll_ImmediateCancellationOnLeaseLoss(t *testing.T) {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("failed to create eq: %v", err)
	}
	defer eq.Close()

	queue := "/test/cancel"
	_, err = eq.Modify(ctx, entroq.InsertingInto(queue, entroq.WithValue("work")))
	if err != nil {
		t.Fatalf("failed to insert: %v", err)
	}
	// 1. Claim the task.
	claimed, err := eq.Claim(ctx, entroq.From(queue), entroq.ClaimFor(10*time.Second))
	if err != nil {
		t.Fatalf("failed to claim: %v", err)
	}

	// 2. Start renewing.
	errChan := make(chan error, 1)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	go func() {
		// Use a short renewal interval to speed up the test.
		errChan <- worker.DoWithRenewAll(ctx, eq, []*entroq.Task{claimed}, 100*time.Millisecond, func(ctx context.Context, stop worker.FinalizeRenewAll) error {
			// Wait for the context to be canceled from the outside.
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	// 3. Poach the task from underneath the worker.
	// Bump version by claiming or deleting. Same claimant, so no need to spoof.
	if _, err := eq.Modify(ctx, claimed.Delete()); err != nil {
		t.Fatalf("failed to steal task: %v", err)
	}

	// 4. Verify that DoWithRenewAll returns DependencyError (wrapped or direct)
	// and that it happened quickly.
	select {
	case err := <-errChan:
		if _, ok := entroq.AsDependency(err); !ok {
			t.Errorf("expected DependencyError, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Errorf("timed out waiting for worker cancellation after lease loss")
	}
}
