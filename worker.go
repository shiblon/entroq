package entroq

import (
	"context"
	"time"
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

	pollInterval  time.Duration
	renewInterval time.Duration
	leaseTime     time.Duration
	taskTimeout   time.Duration

	// Iterator states.
	ready    <-chan time.Time
	claimCtx context.Context
	task     *Task
	err      error
}

// NewWorker creates a new worker iterator-like type that makes it easy to claim and operate on tasks in a loop.
func (c *EntroQ) NewWorker(q string, opts ...WorkerOption) *Worker {
	w := &Worker{
		Q:             q,
		eqc:           c,
		pollInterval:  30 * time.Second,
		renewInterval: 15 * time.Second,
		taskTimeout:   1 * time.Minute,
		ready:         time.After(0),
	}
	for _, opt := range opts {
		opt(w)
	}
	w.leaseTime = 2 * w.renewInterval
	return w
}

// Err returns the most recent error encountered when doing work for a task.
func (w *Worker) Err() error {
	return w.err
}

// Next attempts to get ready for a new claim.
func (w *Worker) Next(ctx context.Context) bool {
	if w.claimCtx == nil {
		w.claimCtx = ctx
	}
	select {
	case <-w.claimCtx.Done():
		w.err = w.claimCtx.Err()
		return false
	case <-w.ready:
	}

	w.claimCtx, _ = context.WithTimeout(ctx, w.taskTimeout)
	w.ready = time.After(w.pollInterval)

	w.task, w.err = w.eqc.Claim(w.claimCtx, w.Q, w.leaseTime)
	if w.err != nil {
		return false
	}
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

// WorkerOption can be passed to AnalyticWorker to modify the worker
type WorkerOption func(*Worker)

// WithPollInterval changes the polling interval from the default (30 seconds) to a specified value.
func WithPollInterval(d time.Duration) WorkerOption {
	return func(w *Worker) {
		w.pollInterval = d
	}
}

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
