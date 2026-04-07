package entroq

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// ErrQMap is a function that maps from an inbox name to its "move on error"
// error box name. If no mapping is found, a suitable default should be
// returned.
type ErrQMap func(inbox string) string

// DefaultRetryDelay is the amount by which to advance the arrival time when a
// worker task errors out as retryable.
const DefaultRetryDelay = 30 * time.Second

// DefaultBackoff is the default time a worker sleeps after a fundamental
// error (renewal failure, connection error, etc.) before attempting to claim
// again. This prevents tight restart loops under network partitions or
// transient backend outages.
const DefaultBackoff = 10 * time.Second

// WorkHandler is an interface that can be implemented to define work to be done.
type WorkHandler interface {
	// Run is called by Worker.Run for each claimed task. The task is renewed
	// in the background while this function runs.
	//
	// On nil return, renewal is stopped and Finalize (if set) is called with the
	// stable task version.
	//
	// On RetryTaskError or MoveTaskError, the task is retried or moved
	// respectively and Finalize is skipped.
	//
	// On any other error, Finalize is skipped and the error is treated as a
	// fundamental error subject to backoff.
	Run(context.Context, *Task) error

	// Finalize is called after Run returns nil and renewal has stopped.
	// It receives the stable (final renewed) task version. Use it to apply task
	// modifications -- deletion, requeueing, etc. -- and any associated cleanup.
	// Finalize is skipped when RunFunc returns a non-nil error.
	Finalize(context.Context, *Task) error
}

// CompactRun is a function type that allows a work handler to be defined, and
// that handler passes modifications out instead of making those modifications
// itself. It's a convenience for folks that don't need special finalization.
type CompactRun func(context.Context, *Task) ([]ModifyArg, error)

// funcHandler is an implementation for WorkHandler that accepts functions.
type funcHandler struct {
	run      func(context.Context, *Task) error
	finalize func(context.Context, *Task) error
}

// Run runs the specified run function.
func (h *funcHandler) Run(ctx context.Context, task *Task) error {
	if h.run == nil {
		return fmt.Errorf("no run function specified")
	}
	return h.run(ctx, task)
}

// Finalized runs the specified finalizer function if it has been defined.
func (h *funcHandler) Finalize(ctx context.Context, task *Task) error {
	if h.finalize == nil {
		return nil
	}
	return h.finalize(ctx, task)
}

// FuncHandler creates a WorkHandler from run and finalize functions.
// The finalizer function can be nil, but the run function should not be.
func FuncHandler(run func(context.Context, *Task) error, finalize func(context.Context, *Task) error) *funcHandler {
	return &funcHandler{run, finalize}
}

// CompactHandler creates a WorkHandler from a compact run function.
func CompactHandler(eqc *EntroQ, compactRun CompactRun) *funcHandler {
	var mods []ModifyArg
	var initial *Task
	return &funcHandler{
		run: func(ctx context.Context, task *Task) error {
			var err error
			initial = task
			if mods, err = compactRun(ctx, task); err != nil {
				return fmt.Errorf("compact run: %w", err)
			}
			return nil
		},
		finalize: func(ctx context.Context, renewed *Task) error {
			modification := NewModification("", mods...)

			// Handle version surprises and renewals.
			switch {
			case initial.Version > renewed.Version:
				return fmt.Errorf("task updated inside worker body, expected version <= %v, got %v", renewed.Version, initial.Version)
			case initial.Version < renewed.Version:
				// Update it wherever we find it.
				for _, task := range modification.Changes {
					if task.ID == renewed.ID {
						task.Version = renewed.Version
					}
				}
				for _, id := range modification.Depends {
					if id.ID == renewed.ID {
						id.Version = renewed.Version
					}
				}
				for _, id := range modification.Deletes {
					if id.ID == renewed.ID {
						id.Version = renewed.Version
					}
				}
			}

			// Perform the modification.
			if _, _, err := eqc.Modify(ctx, WithModification(modification)); err != nil {
				if _, ok := AsDependency(err); ok {
					log.Printf("Worker ack failed, throwing away: %v", err)
					return nil
				}
				if IsTimeout(err) {
					log.Printf("Worker continuing after ack timeout: %v", err)
					return nil
				}
				if IsCanceled(err) {
					log.Printf("Worker exiting cleanly instead of acking: %v", err)
					return fmt.Errorf("canceled in compact finalize: %w", err)
				}
				return fmt.Errorf("worker ack: %w", err)
			}
			return nil
		},
	}
}

// Worker defines a looping protocol, setting up Run to process tasks in a
// queue. It goes through a claim/work/finalize cycle, where the work section
// has background task auto-renewal happening to allow the worker to maintain
// ownership of the task while it does its job.
//
// The finalization phase stops the renewal, freezes the task version, and
// allows it to be deleted or modified safely.
//
// Example:
//
//	var result []byte
//	w := eqClient.NewWorker(
//	    func(ctx context.Context, task *Task) error {
//	        var err error
//	        result, err = doWork(task.Value)
//	        return err
//	    },
//	    func(ctx context.Context, task *Task) error {
//	        _, _, err := eqClient.Modify(ctx, task.Delete())
//	        return err
//	    },
//	)
//	err := w.Run(ctx, "queue_name")
type Worker struct {
	eqc *EntroQ

	lease          time.Duration
	baseRetryDelay time.Duration // put AT into the future when using RetryTaskError.
	backoff        time.Duration // sleep duration after infrastructure errors before retrying.

	handler WorkHandler

	// ErrQMap maps an inbox to the queue tasks are moved to if a MoveTaskError
	// is returned from a worker's run function.
	ErrQMap ErrQMap

	// MaxAttempts indicates how many attempts are too many before a retryable
	// error becomes permanent and the task is moved to an error queue.
	MaxAttempts int32
}

// MoveTaskError causes a task to be moved to a specified queue. This can be
// useful when non-fatal task-specific errors happen in a worker and we want to
// stash them somewhere instead of just causing the worker to crash, but allows
// us to handle that as an early error return. The error is added to the task.
type MoveTaskError struct {
	Err error
}

// NewMoveTaskError creates a new MoveTaskError from the given error.
func NewMoveTaskError(err error) *MoveTaskError {
	return &MoveTaskError{Err: err}
}

// MoveTaskErrorf creates a MoveTaskError given a format string and values,
// just like fmt.Errorf.
func MoveTaskErrorf(format string, values ...any) *MoveTaskError {
	return NewMoveTaskError(fmt.Errorf(format, values...))
}

// Error produces an error string.
func (e *MoveTaskError) Error() string {
	return e.Err.Error()
}

// Unwrap allows errors.Is and errors.As to see through this error.
func (e *MoveTaskError) Unwrap() error {
	return e.Err
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
	return &RetryTaskError{Err: err}
}

// RetryTaskErrorf creates a RetryTaskError in the same way that you would
// create an error with fmt.Errorf.
func RetryTaskErrorf(format string, values ...any) *RetryTaskError {
	return NewRetryTaskError(fmt.Errorf(format, values...))
}

// Error produces an error string.
func (e *RetryTaskError) Error() string {
	return e.Err.Error()
}

// Unwrap allows errors.Is and errors.As to see through this error.
func (e *RetryTaskError) Unwrap() error {
	return e.Err
}

// NewWorker creates a new worker that makes it easy to claim and operate on
// tasks in an endless loop. The run function is called for each claimed task;
// finalize (may be nil) is called on the stable task after run succeeds.
func NewWorker(eq *EntroQ, handler WorkHandler, opts ...WorkerOption) *Worker {
	w := &Worker{
		ErrQMap: DefaultErrQMap,

		eqc:     eq,
		handler: handler,
		lease:   DefaultClaimDuration,

		baseRetryDelay: DefaultRetryDelay,
		backoff:        DefaultBackoff,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// NewWorker is a convenience method on an EntroQ client to create a worker.
func (c *EntroQ) NewWorker(handler WorkHandler, opts ...WorkerOption) *Worker {
	return NewWorker(c, handler, opts...)
}


// runOne claims one task, runs the work function with renewal, and applies
// any resulting modification. Returns nil on success and on handled task-level
// errors (dependency errors and timeouts are logged and swallowed). Returns a
// non-nil error on work failures or context cancellation.
//
// Claim failures (e.g. from a network partition) are retried after backing
// off, since they are likely transient. Work errors cause immediate return.
func (w *Worker) runOne(ctx context.Context, qs []string) error {
	task, err := w.eqc.Claim(ctx, From(qs...), ClaimFor(w.lease))
	if err != nil {
		if IsCanceled(err) {
			return err
		}
		// Claim failures are likely transient -- back off before returning so
		// the loop doesn't spin tightly under a network partition.
		log.Printf("Worker claim failed (%q), backing off %v: %v", qs, w.backoff, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(w.backoff):
		}
		return nil
	}

	errQ := w.ErrQMap(task.Queue)

	// handleErr is set when run returns a RetryTaskError or MoveTaskError.
	// It is applied after renewal stops using the stable task version.
	var stable *Task
	var handleErr error

	if err := w.eqc.DoWithRenew(ctx, task, w.lease, func(ctx context.Context, stop FinalizeRenew) error {
		defer func() { stable = stop() }()
		if err := w.handler.Run(ctx, task); err != nil {
			if e := new(RetryTaskError); errors.As(err, &e) {
				if w.MaxAttempts == 0 || task.Attempt+1 < w.MaxAttempts {
					log.Printf("Worker received retryable error, incrementing attempt: %v", e)
				} else {
					log.Printf("Worker max attempts reached, moving to %q instead of retrying: %v", errQ, e)
				}
				handleErr = err
				return nil
			}
			if e := new(MoveTaskError); errors.As(err, &e) {
				log.Printf("Worker moving to %q: %v", errQ, err)
				handleErr = err
				return nil
			}
			return fmt.Errorf("work (%q): %w", qs, err)
		}
		return nil
	}); err != nil {
		if _, ok := AsDependency(err); ok {
			log.Printf("Worker continuing after dependency (%q)", qs)
			return nil
		}
		if IsTimeout(err) {
			log.Printf("Worker continuing after timeout (%q)", qs)
			return nil
		}
		return err // including canceled - Run handles that
	}

	// Renewal has stopped; stable holds the final task version.
	// Apply retry/move modification if run signaled one.
	if handleErr != nil {
		var args ModifyArg
		if e := new(RetryTaskError); errors.As(handleErr, &e) {
			args = stable.RetryOrQuarantine(e.Error(), errQ, w.MaxAttempts, ArrivalTimeBy(w.baseRetryDelay))
		} else if e := new(MoveTaskError); errors.As(handleErr, &e) {
			args = stable.Quarantine(e.Error(), errQ)
		}
		if _, _, err := w.eqc.Modify(ctx, args); err != nil {
			if _, ok := AsDependency(err); ok {
				log.Printf("Worker error ack failed (%q), throwing away: %v", qs, err)
				return nil
			}
			if IsTimeout(err) {
				log.Printf("Worker continuing (%q) after error ack timeout: %v", qs, err)
				return nil
			}
			return err
		}
		return nil
	}

	// Success: call finalize with the stable task.
	if err := w.handler.Finalize(ctx, stable); err != nil {
		if _, ok := AsDependency(err); ok {
			log.Printf("Worker ack failed (%q), throwing away: %v", qs, err)
			return nil
		}
		if IsTimeout(err) {
			log.Printf("Worker continuing (%q) after ack timeout: %v", qs, err)
			return nil
		}
		if IsCanceled(err) {
			log.Printf("Worker exiting cleanly instead of acking: %v", err)
			return fmt.Errorf("canceled in compact finalize: %w", err)
		}
		return fmt.Errorf("worker ack (%q): %w", qs, err)
	}
	return nil
}

// Run claims tasks from the given queues and processes them in a loop until
// the context is canceled or an unrecoverable error is encountered. Transient
// claim failures are backed off inside runOne; work errors cause immediate exit.
func (w *Worker) Run(ctx context.Context, qs ...string) error {
	if len(qs) == 0 {
		return fmt.Errorf("no queues specified to work on")
	}

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("worker quit: %w", ctx.Err())
		default:
		}

		if err := w.runOne(ctx, qs); err != nil {
			if IsCanceled(err) {
				return nil
			}
			return fmt.Errorf("worker (%q): %w", qs, err)
		}
	}
}

// WorkerOption can be passed to NewWorker to modify parameters.
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

// WithBackoff sets how long the worker sleeps after an infrastructure
// error (renewal failure, connection error, etc.) before attempting to claim
// again. Defaults to DefaultBackoff. Set to 0 to disable backoff.
func WithBackoff(d time.Duration) WorkerOption {
	return func(w *Worker) {
		w.backoff = d
	}
}

// DefaultErrQMap appends "/err" to the inbox, and is the default behavior if
// no overriding error queue mapping options are provided.
func DefaultErrQMap(inbox string) string {
	return inbox + "/err"
}
