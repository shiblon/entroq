package entroq

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"
)

const DefaultRetryDelay = 30 * time.Second

// ErrQMap is a function that maps from an inbox name to its "move on error"
// error box name. If no mapping is found, a suitable default should be
// returned.
type ErrQMap func(inbox string) string

// DependencyHandler is called (if set) when a worker run finishes with a
// dependency error. If it returns a non-nil error, that converts into a fatal
// error.
type DependencyHandler func(err DependencyError) error

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
	// Qs contains the queues to work on.
	Qs []string

	// ErrQMap maps an inbox to the queue tasks are moved to if a MoveTaskError
	// is returned from a worker's run function.
	ErrQMap ErrQMap

	// OnDepErr can hold a function to be called when a dependency error is
	// encountered. if it returns a non-nil error, it will become fatal.
	OnDepErr DependencyHandler

	// MaxAttempts indicates how many attempts are too many before a retryable
	// error becomes permanent and the task is moved to an error queue.
	MaxAttempts int32

	eqc *EntroQ

	lease          time.Duration
	wrappedMove    bool          // whether to wrap a moved error task or use the attempt/err fields.
	baseRetryDelay time.Duration // put AT into the future when using RetryTaskError.
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
	return e.Err.Error()
}

// AsMoveTaskError returns the underlying error and true iff the underlying
// error indicates a worker task should be moved to the error queue instead o
// causing the worker to exit.
func AsMoveTaskError(err error) (*MoveTaskError, bool) {
	cause := errors.Cause(err)
	mte, ok := cause.(*MoveTaskError)
	return mte, ok
}

// ErrorTaskValue holds a task that is moved to an error queue, with an error
// message attached.
type ErrorTaskValue struct {
	Task *Task  `json:"task"`
	Err  string `json:"err"`
}

// RetryTaskError causes a task to be retried, incrementing its Attempt field
// and setting its Err to the text of the error. If MaxAttempts is positive and
// nonzero, and has been reached, then this behaves in the same ways as a
// MoveTaskError.
type RetryTaskError struct {
	Err error
}

// NewRetryTaskError creates a new RetryTaskError from the given error.
func NewRetryTaskError(err error) *RetryTaskError {
	return &RetryTaskError{err}
}

// Error produces an error string.
func (e *RetryTaskError) Error() string {
	return e.Err.Error()
}

// AsRetryTaskError returns the underlying error and true iff the underlying
// error is a retry error.
func AsRetryTaskError(err error) (*RetryTaskError, bool) {
	cause := errors.Cause(err)
	rte, ok := cause.(*RetryTaskError)
	return rte, ok
}

// NewWorker creates a new worker that makes it easy to claim and operate on
// tasks in an endless loop.
func NewWorker(eq *EntroQ, qs ...string) *Worker {
	return &Worker{
		Qs:      qs,
		ErrQMap: DefaultErrQMap,

		eqc:   eq,
		lease: DefaultClaimDuration,

		baseRetryDelay: DefaultRetryDelay,
	}
}

// NewWorker is a convenience method on an EntroQ client to create a worker.
func (c *EntroQ) NewWorker(qs ...string) *Worker {
	return NewWorker(c, qs...)
}

func (w *Worker) WithOpts(opts ...WorkerOption) *Worker {
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Work is a function that is called by Run. It does work for one task, then
// returns any necessary modifications.
//
// If this function returns a MoveTaskError, the original task is moved into
// a queue specified by calling ErrQMap on the original queue name.
// This is useful for keeping track of failed tasks by moving them out of the
// way instead of deleting them or allowing them to be picked up again.
//
// If this function returns a RetryTaskError, the original task has its attempt
// field incremented, the err field is updated to contain the text of the
// error, and the worker goes around again, leaving it to be reclaimed. If the
// maximum number of attempts has been reached, however, the error acts like a
// MoveTaskError, instead.
type Work func(ctx context.Context, task *Task) ([]ModifyArg, error)

func (w *Worker) moveTaskWithError(ctx context.Context, task *Task, newQ string, taskErr error, incrementAttempt bool) error {
	if w.wrappedMove {
		if incrementAttempt {
			task.Attempt++
		}
		newVal, err := json.Marshal(&ErrorTaskValue{Task: task, Err: taskErr.Error()})
		if err != nil {
			return errors.Wrapf(err, "trying to marshal movable task with own error: %q", taskErr)
		}
		if _, _, err := w.eqc.Modify(ctx, task.AsDeletion(), InsertingInto(newQ, WithValue(newVal))); err != nil {
			return errors.Wrapf(err, "trying to insert movable task with own error: %q", taskErr)
		}
		return nil
	}
	changeArgs := []ChangeArg{QueueTo(newQ), ErrTo(errors.Cause(taskErr).Error())}
	if incrementAttempt {
		changeArgs = append(changeArgs, AttemptToNext())
	}
	if _, _, err := w.eqc.Modify(ctx, task.AsChange(changeArgs...)); err != nil {
		return errors.Wrapf(err, "trying to modify attempts and error message for moving task: %q", taskErr)
	}
	return nil
}

// Run attempts to run the given function once per each claimed task, in a
// loop, until the context is canceled or an unrecoverable error is
// encountered. The function can return modifications that should be done after
// it exits, and version numbers for claim renewals will be automatically
// updated.
func (w *Worker) Run(ctx context.Context, f Work) (err error) {
	if len(w.Qs) == 0 {
		return errors.New("No queues specified to work on")
	}
	defer func() {
		log.Printf("Finishing EntroQ worker %q on client %v: err=%v", w.Qs, w.eqc.ID(), err)
	}()
	log.Printf("Starting EntroQ worker %q on client %v, leasing for %v at a time", w.Qs, w.eqc.ID(), w.lease)

	for {
		select {
		case <-ctx.Done():
			return errors.Wrapf(ctx.Err(), "worker quit (%q)", w.Qs)
		default:
		}

		task, err := w.eqc.Claim(ctx, From(w.Qs...), ClaimFor(w.lease))
		if err != nil {
			return errors.Wrapf(err, "worker claim (%q)", w.Qs)
		}

		errQ := w.ErrQMap(task.Queue)

		var args []ModifyArg
		renewed, workErr := w.eqc.DoWithRenew(ctx, task, w.lease, func(ctx context.Context) error {
			var err error
			if args, err = f(ctx, task); err != nil {
				return errors.Wrapf(err, "worker run with renew (%q)", w.Qs)
			}
			return nil
		})
		if workErr != nil {
			log.Printf("Worker error (%q): %v", w.Qs, workErr)
			if _, ok := AsDependency(workErr); ok {
				log.Printf("Worker continuing after dependency (%q)", w.Qs)
				continue
			}
			if IsTimeout(workErr) {
				log.Printf("Worker continuing after timeout (%q)", w.Qs)
				continue
			}
			if IsCanceled(workErr) {
				log.Printf("Worker shutting down cleanly (%q)", w.Qs)
				return nil
			}
			if _, ok := AsRetryTaskError(workErr); ok {
				log.Printf("Worker received retryable error, incrementing attempt: %v", workErr)
				if w.MaxAttempts > 0 && task.Attempt+1 >= w.MaxAttempts {
					// Move instead - we retried enough times already.
					log.Printf("Worker max attempts reached, moving to %q instead of retrying: %v", errQ, workErr)
					if err := w.moveTaskWithError(ctx, task, errQ, workErr, true); err != nil {
						return errors.Wrap(err, "move work task instead of retry")
					}
				} else {
					// Can retry. Increment attempts and move on.
					if _, _, err := w.eqc.Modify(ctx, task.AsChange(
						ErrTo(errors.Cause(workErr).Error()),
						AttemptToNext(),
						ArrivalTimeBy(w.baseRetryDelay))); err != nil {
						return errors.Wrap(err, "retry task")
					}
				}
				continue
			}
			if _, ok := AsMoveTaskError(workErr); ok {
				log.Printf("Worker moving error task to %q instead of exiting: %v", errQ, workErr)
				if err := w.moveTaskWithError(ctx, task, errQ, workErr, false); err != nil {
					return errors.Wrap(err, "move work task")
				}
				continue
			}
			return errors.Wrapf(workErr, "worker error (%q)", w.Qs)
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
			if depErr, ok := AsDependency(err); ok {
				if w.OnDepErr != nil {
					if err := w.OnDepErr(depErr); err != nil {
						log.Printf("Dependency error upgraded to fatal: %v", err)
						return errors.Wrap(err, "worker depdency error upgraded to fatal")
					}
				}
				log.Printf("Worker ack failed (%q), throwing away: %v", w.Qs, err)
				continue
			}
			if IsTimeout(err) {
				log.Printf("Worker continuing (%q) after ack timeout: %v", w.Qs, err)
				continue
			}
			if IsCanceled(err) {
				log.Printf("Worker exiting cleanly (%q) instead of acking: %v", w.Qs, err)
				return nil
			}
			return errors.Wrapf(err, "worker ack (%q)", w.Qs)
		}
	}
}

// WorkerOption can be passed to AnalyticWorker to modify the worker
type WorkerOption func(*Worker)

// WithLease sets the frequency of task renewal. Tasks will be claimed
// for an amount of time slightly longer than this so that they have a chance
// of being renewed before expiring.
func WithLease(d time.Duration) WorkerOption {
	return func(w *Worker) {
		w.lease = d
	}
}

// WithErrQMap sets a function that maps from inbox queue names to error queue names.
// Defaults to DefaultErrQMap.
func WithErrQMap(f ErrQMap) WorkerOption {
	return func(w *Worker) {
		w.ErrQMap = f
	}
}

// WithDependencyHandler sets a function to be called when a worker
// encounters a dependency error. If this function returns a non-nil error, the
// worker will exit.
//
// Note that workers always exit on non-dependency errors, but usually treat
// dependency errors as things that can be retried. Specifying a handler for
// dependency errors allows different behavior as needed.
//
// One possible use case for a dependency error handler is to reload a
// configuration task for the next round: if the task is depended on, but has
// been changed, the task can be retried, but configuration should also be
// reloaded, which could be done in a handler.
func WithDependencyHandler(f DependencyHandler) WorkerOption {
	return func(w *Worker) {
		w.OnDepErr = f
	}
}

// WithMaxAttempts sets the maximum attempts that are allowed before a
// RetryTaskError turns into a MoveTaskError (transparently). If this value is
// 0 (the default), then there is no maximum, and attempts can be incremented
// indefinitely without a move to an error queue.
func WithMaxAttempts(m int32) WorkerOption {
	return func(w *Worker) {
		w.MaxAttempts = m
	}
}

// WithBaseRetryDelay sets the base delay for a retried task (the first
// attempt). Without any backoff settings, this is used for every retry. When
// used, the task is modified when its attempt is incremented to have its
// availabiliy time incremented by this amount from now.
func WithBaseRetryDelay(d time.Duration) WorkerOption {
	return func(w *Worker) {
		w.baseRetryDelay = d
	}
}

// WithWrappedMove changes behavior of a MoveTaskError to wrap the entire task
// into a brand new error task, where the old task is serialized into bytes and
// stored as the new tas's value. The default is to use Attempt and Err to
// store necessary data in the existing task, instead.
func WithWrappedMove(on bool) WorkerOption {
	return func(w *Worker) {
		w.wrappedMove = on
	}
}

// DefaultErrQMap appends "/err" to the inbox, and is the default behavior if
// no overriding error queue mapping options are provided.
func DefaultErrQMap(inbox string) string {
	return inbox + "/err"
}
