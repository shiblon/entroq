package entroq

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path"
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
//	w := eqClient.NewWorker("queue_name")
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
	Q string

	// ErrQ is the queue where "moved" tasks go, when an error is recoverable
	// but task information should be saved elsewhere.
	ErrQ string

	eqc *EntroQ

	renew time.Duration
	lease time.Duration
}

// MoveTaskError causes a task to be completely serialized, wrapped in a
// larger JSON object with error information, and moved to a specified queue.
// This can be useful when non-fatal task-specific errors happen in a worker
// and we want to stash them somewhere instead of just causing the worker to
// crash, but allows us to handle that as an early error return.
type MoveTaskError struct {
	Err error
}

// NewMoveTaskError creates a new MoveTaskError from the given error.
func NewMoveTaskError(err error) *MoveTaskError {
	return &MoveTaskError{err}
}

// Error produces an error string.
func (e *MoveTaskError) Error() string {
	return fmt.Sprintf("task-specific, movable: %v", e.Err)
}

// AsMoveTaskError returns the underlying error and true iff the underlying
// error indicates a worker task should be moved to the error queue instead o
// causing the worker to exit.
func AsMoveTaskError(err error) (*MoveTaskError, bool) {
	cause := errors.Cause(err)
	mte, ok := cause.(*MoveTaskError)
	return mte, ok
}

// ErrorTaskValue holds a task that is moved to an error queue, with an error message attached.
type ErrorTaskValue struct {
	Task *Task  `json:"task"`
	Err  string `json:"err"`
}

// NewWorker creates a new worker iterator-like type that makes it easy to claim and operate on tasks in a loop.
func (c *EntroQ) NewWorker(q string, opts ...WorkerOption) *Worker {
	w := &Worker{
		Q:     q,
		ErrQ:  path.Join(q, "err"),
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
func (w *Worker) Run(ctx context.Context, f func(ctx context.Context, task *Task) ([]ModifyArg, error)) (err error) {
	defer func() {
		log.Printf("Finishing EntroQ worker %q on client %v: err=%v", w.Q, w.eqc.ID(), err)
	}()
	log.Printf("Starting EntroQ worker %q on client %v", w.Q, w.eqc.ID())
	for {
		select {
		case <-ctx.Done():
			return errors.Wrapf(ctx.Err(), "worker quit (%q)", w.Q)
		default:
		}

		task, err := w.eqc.Claim(ctx, w.Q, w.lease)
		if err != nil {
			return errors.Wrapf(err, "worker claim (%q)", w.Q)
		}

		var args []ModifyArg
		renewed, err := w.eqc.DoWithRenew(ctx, task, w.lease, func(ctx context.Context) error {
			var err error
			if args, err = f(ctx, task); err != nil {
				return errors.Wrapf(err, "worker run with renew (%q)", w.Q)
			}
			return nil
		})

		if err != nil {
			log.Printf("Worker error (%q): %T %v", w.Q, err, err)
			if _, ok := AsDependency(err); ok {
				log.Printf("Worker continuing after dependency (%q)", w.Q)
				continue
			}
			if IsTimeout(err) {
				log.Printf("Worker continuing after timeout (%q)", w.Q)
				continue
			}
			if IsCanceled(err) {
				log.Printf("Worker shutting down cleanly (%q)", w.Q)
				return nil
			}
			if _, ok := AsMoveTaskError(err); ok {
				log.Printf("Worker moving error task to %q instead of exiting: %v", w.ErrQ, err)
				newVal, marshalErr := json.Marshal(&ErrorTaskValue{Task: task, Err: err.Error()})
				if marshalErr != nil {
					return errors.Wrapf(marshalErr, "trying to marshal movable task with own error: %q", err)
				}
				if _, _, insErr := w.eqc.Modify(ctx, task.AsDeletion(), InsertingInto(w.ErrQ, WithValue(newVal))); err != nil {
					return errors.Wrapf(insErr, "trying to insert movable task with own error: %q", err)
				}
				return nil
			}

			return errors.Wrapf(err, "worker error (%q)", w.Q)
		}

		modification := NewModification(uuid.Nil, args...)
		for _, task := range modification.Changes {
			if task.ID == renewed.ID && task.Version != renewed.Version {
				if task.Version > renewed.Version {
					return errors.Errorf("task updated inside worker body, expected version <= %v, got %v", renewed.Version, task.Version)
				}
				log.Printf("Rewriting change version %v => %v", task.Version, renewed.Version)
				task.Version = renewed.Version
			}
		}
		for _, id := range modification.Depends {
			if id.ID == renewed.ID && task.Version != renewed.Version {
				if task.Version > renewed.Version {
					return errors.Errorf("task updated inside worker body, expected version <= %v, got %v", renewed.Version, task.Version)
				}
				log.Printf("Rewriting depend version %v => %v", task.Version, renewed.Version)
				id.Version = renewed.Version
			}
		}
		for _, id := range modification.Deletes {
			if id.ID == renewed.ID && task.Version != renewed.Version {
				if task.Version > renewed.Version {
					return errors.Errorf("task updated inside worker body, expected version <= %v, got %v", renewed.Version, task.Version)
				}
				log.Printf("Rewriting delete version %v => %v", task.Version, renewed.Version)
				id.Version = renewed.Version
			}
		}

		if _, _, err := w.eqc.Modify(ctx, WithModification(modification)); err != nil {
			if _, ok := AsDependency(err); ok {
				log.Printf("Worker ack failed (%q), throwing away: %v", w.Q, err)
				continue
			}
			if IsTimeout(err) {
				log.Printf("Worker continuing (%q) after ack timeout: %v", w.Q, err)
				continue
			}
			if IsCanceled(err) {
				log.Printf("Worker exiting cleanly (%q) instead of acking: %v", w.Q, err)
				return nil
			}
			return errors.Wrapf(err, "worker ack (%q)", w.Q)
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

// WithErrorQueue sets the error queue for a "moved" task (as opposed
// to one that errors out causing a crash). The default is to append "/err" to
// the inbox value.
func WithErrorQueue(q string) WorkerOption {
	return func(w *Worker) {
		w.ErrQ = q
	}
}
