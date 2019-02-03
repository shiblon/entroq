package entroq

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Worker creates an iterator-like protocol for processing tasks in a queue,
// one at a time, in a loop. Each worker should only be accessed from only a
// single goroutine. If multiple goroutines are desired, they should each use
// their own worker instance.
//
// Example:
//   w := NewWorker(client, "queue_name")
//   for w.Next(ctx) {
//     task, err := w.Do(func(ctx context.Context) error {
//     	 // The claimed task is held in w.Task(), and its value will not change
//     	 // while this function runs. The final renewed task is returned from Do.
//       log.Printf("Task value:\n%s", string(w.Task().Value))
//
//     	 // ...do work on w.Task() that might take a while...
//     })
//     if err = nil {
//       log.Printf("Error for claimed task: %v", err)
//       continue
//     }
//
//     // Potentially do something with the returned (potentially renewed) task
//     // value, e.g., delete it because it's finished:
//     if _, _, err := client.Modify(ctx, task.AsDeletion()); err != nil {
//       log.Printf("Error deleting final task: %v", err)
//       continue
//     }
//   }
//   if err := w.Err(); err != nil {
//     log.Fatalf("Error running worker: %v", err)
//   }
type Worker struct {
	// Q is the queue to claim work from in an endless loop.
	Q   string
	eqc *EntroQ

	renewInterval time.Duration
	leaseTime     time.Duration
	taskTimeout   time.Duration
	done          chan struct{}

	// Iterator states.
	claimCtx context.Context
	task     *Task
	err      error
}

// NewWorker creates a new worker iterator-like type that makes it easy to claim and operate on tasks in a loop.
func (c *EntroQ) NewWorker(q string, opts ...WorkerOption) *Worker {
	w := &Worker{
		Q:             q,
		eqc:           c,
		renewInterval: 15 * time.Second,
		done:          make(chan struct{}),
	}
	for _, opt := range opts {
		opt(w)
	}
	w.leaseTime = 2 * w.renewInterval
	return w
}

// Stop causes the loop to terminate on the next call to Next.
func (w *Worker) Stop() {
	close(w.done)
}

// Err returns the most recent error encountered when doing work for a task.
func (w *Worker) Err() error {
	return w.err
}

// Next attempts to get ready for a new claim.
func (w *Worker) Next(ctx context.Context) bool {
	select {
	case <-w.done:
		return false
	case <-ctx.Done():
		w.err = ctx.Err()
		return false
	default:
	}

	w.task, w.err = w.eqc.Claim(ctx, w.Q, w.leaseTime)
	if w.err != nil {
		return false
	}
	w.claimCtx = ctx
	return true
}

// Task returns the claimed task for this worker.
func (w *Worker) Task() *Task {
	return w.task
}

// Ctx returns the current task claim's context (valid during a call to Do).
func (w *Worker) Ctx() context.Context {
	return w.claimCtx
}

// Do calls the given function while renewing the claim according to given options.
func (w *Worker) Do(f func(context.Context) error) (*Task, error) {
	return w.eqc.DoWithRenew(w.claimCtx, w.task, w.leaseTime, f)
}

// DoModify calls the given function while renewing the claim, and applies
// requested modifications when finished. If any modifications reference the
// renewed task, they will be altered to reflect the latest version of that
// task, but otherwise left intact.
//
// Returns the result of the final call to Modify.
func (w *Worker) DoModify(f func(context.Context) ([]ModifyArg, error)) (inserted []*Task, changed []*Task, err error) {
	var args []ModifyArg
	renewedTask, err := w.Do(func(ctx context.Context) (err error) {
		args, err = f(ctx)
		return err
	})
	if err != nil {
		return nil, nil, err
	}

	// Find any modifications that reference the renewed task's ID (but not
	// necessarily version, since that might have changed during renewal), and
	// update to the latest known claimed version. Claimant is ignored when
	// passing an entire modification into Modify, so we set it to nil, here.
	modification := NewModification(uuid.Nil, args...)
	for _, task := range modification.Changes {
		if task.ID == renewedTask.ID {
			task.Version = renewedTask.Version
		}
	}
	for _, id := range modification.Depends {
		if id.ID == renewedTask.ID {
			id.Version = renewedTask.Version
		}
	}
	for _, id := range modification.Deletes {
		if id.ID == renewedTask.ID {
			id.Version = renewedTask.Version
		}
	}

	return w.eqc.Modify(w.claimCtx, WithModification(modification))
}

// WorkerOption can be passed to AnalyticWorker to modify the worker
type WorkerOption func(*Worker)

// WithRenewInterval sets the frequency of task renewal. Tasks will be claimed
// for an amount of time slightly longer than this so that they have a chance
// of being renewed before expiring.
func WithRenewInterval(d time.Duration) WorkerOption {
	return func(w *Worker) {
		w.renewInterval = d
	}
}

// WithTaskTimeout sets the overall timeout for a task. If it has been held
// (and renewed) for this long, it will be allowed to expire.
func WithTaskTimeout(d time.Duration) WorkerOption {
	return func(w *Worker) {
		w.taskTimeout = d
	}
}
