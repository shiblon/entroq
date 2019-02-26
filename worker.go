package entroq

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"
)

// Worker creates an iterator-like protocol for processing tasks in a queue,
// one at a time, in a loop. Each worker should only be accessed from a single
// goroutine. If multiple goroutines are desired, they should each use their
// own worker instance.
//
// Example:
//	w := NewWorker("queue_name")
//	err := w.Run(ctx, func(ctx context.Context, task *Task) ([]ModifyArg, error) {
//		// Do stuff with the task.
//		// It's safe to mark it for deletion, too. It is renewed in the background.
//		// If renewal changed its version, that is rewritten before modification.
//		return []ModifyArg{task.AsDeletion()}, nil
//	})
//	// Handle the error, which is nil if the context was canceled (but not if
//	// it timed out).
type Worker struct {
	// Q is the queue to claim work from in an endless loop.
	Q   string
	eqc *EntroQ

	renew time.Duration
	lease time.Duration
}

// NewWorker creates a new worker iterator-like type that makes it easy to claim and operate on tasks in a loop.
func (c *EntroQ) NewWorker(q string, opts ...WorkerOption) *Worker {
	w := &Worker{
		Q:     q,
		eqc:   c,
		lease: 30 * time.Second,
	}
	for _, opt := range opts {
		opt(w)
	}
	w.renew = w.lease / 2
	return w
}

// Run attempts to run the given function once per each claimed task, in a
// loop, until the context is canceled or an unrecoverable error is
// encountered. The function can return modifications that should be done after
// it exits, and version numbers for claim renewals will be automatically
// updated.
func (w *Worker) Run(ctx context.Context, f func(ctx context.Context, task *Task) ([]ModifyArg, error)) error {
	for {
		select {
		case <-ctx.Done():
			return errors.Wrap(ctx.Err(), "worker quit")
		default:
		}

		task, err := w.eqc.Claim(ctx, w.Q, w.lease)
		if err != nil {
			return errors.Wrap(err, "worker claim")
		}

		var args []ModifyArg
		renewed, err := w.eqc.DoWithRenew(ctx, task, w.lease, func(ctx context.Context) error {
			var err error
			if args, err = f(ctx, task); err != nil {
				return errors.Wrap(err, "worker run with renew")
			}
			return nil
		})
		if err != nil {
			if _, ok := AsDependency(err); ok {
				log.Printf("Worker continuing after dependency: %v", err)
				continue
			}
			if IsTimeout(err) {
				log.Printf("Worker continuing after timeout: %v", err)
				continue
			}
			if IsCanceled(err) {
				log.Printf("Worker shutting down cleanly: %v", err)
				return nil
			}
			return errors.Wrap(err, "worker error")
		}

		modification := NewModification(uuid.Nil, args...)
		for _, task := range modification.Changes {
			if task.ID == renewed.ID && task.Version != renewed.Version {
				log.Printf("Rewriting change version %v => %v", task.Version, renewed.Version)
				task.Version = renewed.Version
			}
		}
		for _, id := range modification.Depends {
			if id.ID == renewed.ID && task.Version != renewed.Version {
				log.Printf("Rewriting depend version %v => %v", task.Version, renewed.Version)
				id.Version = renewed.Version
			}
		}
		for _, id := range modification.Deletes {
			if id.ID == renewed.ID && task.Version != renewed.Version {
				log.Printf("Rewriting delete version %v => %v", task.Version, renewed.Version)
				id.Version = renewed.Version
			}
		}

		if _, _, err := w.eqc.Modify(ctx, WithModification(modification)); err != nil {
			if _, ok := AsDependency(err); ok {
				log.Printf("Worker ack failed, throwing away: %v", err)
				continue
			}
			if IsTimeout(err) {
				log.Printf("Worker continuing after ack timeout: %v", err)
				continue
			}
			if IsCanceled(err) {
				log.Printf("Worker exiting cleanly instead of acking: %v", err)
				return nil
			}
			return errors.Wrap(err, "worker ack")
		}
	}
}

// WorkerOption can be passed to AnalyticWorker to modify the worker
type WorkerOption func(*Worker)

// WithRenewInterval sets the frequency of task renewal. Tasks will be claimed
// for an amount of time slightly longer than this so that they have a chance
// of being renewed before expiring.
func WithLease(d time.Duration) WorkerOption {
	return func(w *Worker) {
		w.lease = d
	}
}
