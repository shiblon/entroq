package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/shiblon/entroq"
	"golang.org/x/sync/errgroup"
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

// Handler is an interface that can be implemented to define work to be done.
type Handler interface {
	// TaskDo is called by Worker.Run for each claimed task. The task is renewed
	// in the background while this function runs.
	//
	// On nil return, renewal is stopped and TaskFinish (if set) is called with the
	// stable task version.
	//
	// On RetryError or MoveError, the task is retried or moved
	// respectively and TaskFinish is skipped.
	//
	// On any other error, TaskFinish is skipped and the error is treated as a
	// fundamental error subject to backoff.
	TaskDo(context.Context, *entroq.Task) error

	// TaskFinish is called after TaskDo returns nil and renewal has stopped.
	// It receives the stable (final renewed) task version. Use it to apply task
	// modifications -- deletion, requeueing, etc. -- and any associated cleanup.
	// TaskFinish is skipped when TaskDo returns a non-nil error.
	TaskFinish(context.Context, *entroq.Task) error
}

// DoModifyRun is a function type that allows a work handler to be defined, and
// that handler passes modifications out instead of making those modifications
// itself. It's a convenience for folks that don't need special finalization.
type DoModifyRun func(context.Context, *entroq.Task) ([]entroq.ModifyArg, error)

// funcHandler is an implementation for Handler that accepts functions.
type funcHandler struct {
	do     func(context.Context, *entroq.Task) error
	finish func(context.Context, *entroq.Task) error
}

// TaskDo runs the specified "do" function.
func (h *funcHandler) TaskDo(ctx context.Context, task *entroq.Task) error {
	if h.do == nil {
		return fmt.Errorf("no work function specified")
	}
	return h.do(ctx, task)
}

// TaskFinish runs the specified "finish" function if it has been defined.
func (h *funcHandler) TaskFinish(ctx context.Context, task *entroq.Task) error {
	if h.finish == nil {
		return nil
	}
	return h.finish(ctx, task)
}

// compactHandler is an implementation for Handler that manages DoModify logic.
type compactHandler struct {
	eqc *entroq.EntroQ
	run DoModifyRun

	// State for a single task execution.
	mods    []entroq.ModifyArg
	initial *entroq.Task
}

// TaskDo runs the "run" function and stores resulting modifications.
func (h *compactHandler) TaskDo(ctx context.Context, task *entroq.Task) error {
	var err error
	h.initial = task
	if h.mods, err = h.run(ctx, task); err != nil {
		return fmt.Errorf("do modify: %w", err)
	}
	return nil
}

// TaskFinish applies the modifications captured in TaskDo.
func (h *compactHandler) TaskFinish(ctx context.Context, renewed *entroq.Task) error {
	modification := entroq.NewModification("", h.mods...)

	// Handle version surprises and renewals.
	switch {
	case h.initial.Version > renewed.Version:
		return fmt.Errorf("task updated inside worker body, expected version <= %v, got %v", renewed.Version, h.initial.Version)
	case h.initial.Version < renewed.Version:
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
	if _, _, err := h.eqc.Modify(ctx, entroq.WithModification(modification)); err != nil {
		if _, ok := entroq.AsDependency(err); ok {
			log.Printf("Worker ack failed, throwing away: %v", err)
			return nil
		}
		if entroq.IsTimeout(err) {
			log.Printf("Worker continuing after ack timeout: %v", err)
			return nil
		}
		if entroq.IsCanceled(err) {
			log.Printf("Worker exiting cleanly instead of acking: %v", err)
			return fmt.Errorf("canceled in compact finish: %w", err)
		}
		return fmt.Errorf("worker ack: %w", err)
	}
	return nil
}

// New creates a new worker that makes it easy to claim and operate on
// tasks in an endless loop. Define its logic using options like WithDo,
// WithFinish, or WithDoModify.
func New(eq *entroq.EntroQ, opts ...Option) *Worker {
	w := &Worker{
		ErrQMap: DefaultErrQMap,

		eqc:   eq,
		lease: entroq.DefaultClaimDuration,

		baseRetryDelay: DefaultRetryDelay,
		backoff:        DefaultBackoff,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// WithDo sets the primary work function for a worker. Overwrites any previous
// DoModify configuration.
func WithDo(f func(context.Context, *entroq.Task) error) Option {
	return func(w *Worker) {
		if fh, ok := w.handler.(*funcHandler); ok {
			fh.do = f
		} else {
			// Overwrite previous handler type entirely.
			w.handler = &funcHandler{do: f}
		}
	}
}

// WithFinish sets the finalization/ack function for a worker, which is called
// after Do completes successfully and renewal has stopped. Overwrites any
// previous DoModify configuration.
func WithFinish(f func(context.Context, *entroq.Task) error) Option {
	return func(w *Worker) {
		if fh, ok := w.handler.(*funcHandler); ok {
			fh.finish = f
		} else {
			// Overwrite previous handler type entirely.
			w.handler = &funcHandler{finish: f}
		}
	}
}

// WithDoModify sets a combined work and modification function. It returns a
// list of modifications to be applied after the work is complete. Overwrites
// any previous configuration.
func WithDoModify(f DoModifyRun) Option {
	return func(w *Worker) {
		w.handler = &compactHandler{
			eqc: w.eqc,
			run: f,
		}
	}
}

// WithHandler sets a custom task handler for a worker. Only one handler can be
// active.
func WithHandler(h Handler) Option {
	return func(w *Worker) {
		w.handler = h
	}
}

// Worker defines a looping protocol, setting up Run to process tasks in a
// queue. It goes through a claim/work/finalize cycle, where the work section
// has background task auto-renewal happening to allow the worker to maintain
// ownership of the task while it does its job.
//
// The finalization phase stops the renewal, freezes the task version, and
// allows it to be deleted or modified safely.
type Worker struct {
	eqc *entroq.EntroQ

	lease          time.Duration
	baseRetryDelay time.Duration // put AT into the future when using RetryError.
	backoff        time.Duration // sleep duration after infrastructure errors before retrying.

	handler Handler

	// ErrQMap maps an inbox to the queue tasks are moved to if a MoveError
	// is returned from a worker's run function.
	ErrQMap ErrQMap

	// MaxAttempts indicates how many attempts are too many before a retryable
	// error becomes permanent and the task is moved to an error queue.
	MaxAttempts int32
}

// MoveError causes a task to be moved to a specified queue. This can be
// useful when non-fatal task-specific errors happen in a worker and we want to
// stash them somewhere instead of just causing the worker to crash, but allows
// us to handle that as an early error return. The error is added to the task.
type MoveError struct {
	Err error
}

// NewMoveError creates a new MoveError from the given error.
func NewMoveError(err error) *MoveError {
	return &MoveError{Err: err}
}

// MoveErrorf creates a MoveError given a format string and values,
// just like fmt.Errorf.
func MoveErrorf(format string, values ...any) *MoveError {
	return NewMoveError(fmt.Errorf(format, values...))
}

// Error produces an error string.
func (e *MoveError) Error() string {
	return e.Err.Error()
}

// Unwrap allows errors.Is and errors.As to see through this error.
func (e *MoveError) Unwrap() error {
	return e.Err
}

// RetryError causes a task to be retried, incrementing its Attempt field
// and setting its Err to the text of the error. If MaxAttempts is positive and
// nonzero, and has been reached, then this behaves in the same ways as a
// MoveError.
type RetryError struct {
	Err error
}

// NewRetryError creates a new RetryError from the given error.
func NewRetryError(err error) *RetryError {
	return &RetryError{Err: err}
}

// RetryErrorf creates a RetryError in the same way that you would
// create an error with fmt.Errorf.
func RetryErrorf(format string, values ...any) *RetryError {
	return NewRetryError(fmt.Errorf(format, values...))
}

// Error produces an error string.
func (e *RetryError) Error() string {
	return e.Err.Error()
}

// Unwrap allows errors.Is and errors.As to see through this error.
func (e *RetryError) Unwrap() error {
	return e.Err
}

// Logic is now injected via functional options.

// runOne claims one task, runs the work function with renewal, and applies
// any resulting modification. Returns nil on success and on handled task-level
// errors (dependency errors and timeouts are logged and swallowed). Returns a
// non-nil error on work failures or context cancellation.
func (w *Worker) runOne(ctx context.Context, qs []string) error {
	task, err := w.eqc.Claim(ctx, entroq.From(qs...), entroq.ClaimFor(w.lease))
	if err != nil {
		if entroq.IsCanceled(err) {
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

	// handleErr is set when run returns a RetryError or MoveError.
	// It is applied after renewal stops using the stable task version.
	var stable *entroq.Task
	var handleErr error

	if err := DoWithRenew(ctx, w.eqc, task, w.lease, func(ctx context.Context, stop FinalizeRenew) error {
		defer func() { stable = stop() }()
		if err := w.handler.TaskDo(ctx, task); err != nil {
			if e := new(RetryError); errors.As(err, &e) {
				if w.MaxAttempts == 0 || task.Attempt+1 < w.MaxAttempts {
					log.Printf("Worker received retryable error, incrementing attempt: %v", e)
				} else {
					log.Printf("Worker max attempts reached, moving to %q instead of retrying: %v", errQ, e)
				}
				handleErr = err
				return nil
			}
			if e := new(MoveError); errors.As(err, &e) {
				log.Printf("Worker moving to %q: %v", errQ, err)
				handleErr = err
				return nil
			}
			return fmt.Errorf("work (%q): %w", qs, err)
		}
		return nil
	}); err != nil {
		if _, ok := entroq.AsDependency(err); ok {
			log.Printf("Worker continuing after dependency (%q)", qs)
			return nil
		}
		if entroq.IsTimeout(err) {
			log.Printf("Worker continuing after timeout (%q)", qs)
			return nil
		}
		return err // including canceled - Run handles that
	}

	// Renewal has stopped; stable holds the final task version.
	// Apply retry/move modification if run signaled one.
	if handleErr != nil {
		var args entroq.ModifyArg
		if e := new(RetryError); errors.As(handleErr, &e) {
			args = stable.RetryOrQuarantine(e.Error(), errQ, w.MaxAttempts, entroq.ArrivalTimeBy(w.baseRetryDelay))
		} else if e := new(MoveError); errors.As(handleErr, &e) {
			args = stable.Quarantine(e.Error(), errQ)
		}
		if _, _, err := w.eqc.Modify(ctx, args); err != nil {
			if _, ok := entroq.AsDependency(err); ok {
				log.Printf("Worker error ack failed (%q), throwing away: %v", qs, err)
				return nil
			}
			if entroq.IsTimeout(err) {
				log.Printf("Worker continuing (%q) after error ack timeout: %v", qs, err)
				return nil
			}
			return err
		}
		return nil
	}

	// Success: call task finish with the stable task.
	if err := w.handler.TaskFinish(ctx, stable); err != nil {
		if _, ok := entroq.AsDependency(err); ok {
			log.Printf("Worker ack failed (%q), throwing away: %v", qs, err)
			return nil
		}
		if entroq.IsTimeout(err) {
			log.Printf("Worker continuing (%q) after ack timeout: %v", qs, err)
			return nil
		}
		if entroq.IsCanceled(err) {
			log.Printf("Worker exiting cleanly instead of acking: %v", err)
			return fmt.Errorf("canceled in finish: %w", err)
		}
		return fmt.Errorf("worker ack (%q): %w", qs, err)
	}
	return nil
}

// Run claims tasks from the given queues and processes them in a loop until
// the context is canceled or an unrecoverable error is encountered.
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
			if entroq.IsCanceled(err) {
				return nil
			}
			return fmt.Errorf("worker (%q): %w", qs, err)
		}
	}
}

// Option can be passed to New to modify parameters.
type Option func(*Worker)

// WithLease sets the frequency of task renewal. Tasks will be claimed
// for an amount of time slightly longer than this so that they have a chance
// of being renewed before expiring.
func WithLease(d time.Duration) Option {
	return func(w *Worker) {
		w.lease = d
	}
}

// WithErrQMap sets a function that maps from inbox queue names to error queue names.
// Defaults to DefaultErrQMap.
func WithErrQMap(f ErrQMap) Option {
	return func(w *Worker) {
		w.ErrQMap = f
	}
}

// WithMaxAttempts sets the maximum attempts that are allowed before a
// RetryError turns into a MoveError (transparently). If this value is
// 0 (the default), then there is no maximum.
func WithMaxAttempts(m int32) Option {
	return func(w *Worker) {
		w.MaxAttempts = m
	}
}

// WithBaseRetryDelay sets the base delay for a retried task.
func WithBaseRetryDelay(d time.Duration) Option {
	return func(w *Worker) {
		w.baseRetryDelay = d
	}
}

// WithBackoff sets how long the worker sleeps after an infrastructure
// error before attempting to claim again. Defaults to DefaultBackoff.
func WithBackoff(d time.Duration) Option {
	return func(w *Worker) {
		w.backoff = d
	}
}

// DefaultErrQMap appends "/err" to the inbox.
func DefaultErrQMap(inbox string) string {
	return inbox + "/err"
}

// Renewal Machinery

// FinalizeRenewAll defines a function that can be called to stop renewal from a worker routine.
// It returns a slice of tasks that are no longer being renewed, so versions are stable.
type FinalizeRenewAll func() []*entroq.Task

// FinalizeRenew defines a function that can be called to stop renewal from a worker routine.
// It returns a single task that is no longer being renewed, so its version is stable.
type FinalizeRenew func() *entroq.Task

// DoWorkAll defines a function that accepts a 'stop' function so that work can be
// done, renewal can be stopped to get stable task versions, and cleanup can
// happen. For multiple tasks.
type DoWorkAll func(ctx context.Context, stop FinalizeRenewAll) error

// DoWork defines a function like WorkAll, but handles only one task.
type DoWork func(ctx context.Context, stop FinalizeRenew) error

// DoWithRenewAll runs the provided function while keeping all given tasks leases renewed.
func DoWithRenewAll(ctx context.Context, c *entroq.EntroQ, tasks []*entroq.Task, lease time.Duration, f DoWorkAll) error {
	type outVal struct {
		tasks []*entroq.Task
		err   error
	}
	taskCh := make(chan outVal, 1)

	g, ctx := errgroup.WithContext(ctx)

	fctx, fcancel := context.WithCancelCause(ctx)
	defer fcancel(nil)

	// Run the renewer, with a channel for consuming intermediate renewed tasks.
	stopRenew := make(chan struct{})
	g.Go(func() error {
		renewed := tasks
		var out chan<- outVal
		var stopErr error
		doneCh := ctx.Done()
		for {
			select {
			case <-stopRenew:
				out = taskCh // enable final send
				stopRenew = nil
			case <-doneCh:
				// cancellation shouldn't keep us from returning a value
				out = taskCh
				doneCh = nil
			case <-time.After(lease / 2):
				if stopErr != nil {
					break
				}
				r, err := c.RenewAllFor(ctx, renewed, lease)
				if err != nil {
					if entroq.IsCanceled(err) {
						out = taskCh
						break
					}
					if depErr, ok := entroq.AsDependency(err); ok {
						// Lease lost. Fail fast.
						fcancel(depErr)
						stopErr = depErr
						out = taskCh
						break
					}
					// Transient error. Log and retry at next interval.
					log.Printf("Transient renewal error: %v", err)
					continue
				}
				renewed = r
			case out <- outVal{renewed, stopErr}:
				return nil
			}
		}
	})

	finalize := func() []*entroq.Task {
		close(stopRenew)
		out := <-taskCh
		if out.err != nil {
			fcancel(out.err)
		}
		return out.tasks
	}

	g.Go(func() error {
		if err := f(fctx, finalize); err != nil {
			if errors.Is(err, context.Canceled) {
				if causeErr := context.Cause(fctx); causeErr != nil {
					return fmt.Errorf("work func canceled with error: %w", causeErr)
				}
				return nil
			}
			return fmt.Errorf("renewed user func: %w", err)
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return fmt.Errorf("do with renew all: %w", err)
	}
	return nil
}

// DoWithRenew runs the provided function while keeping the given task lease renewed.
func DoWithRenew(ctx context.Context, c *entroq.EntroQ, task *entroq.Task, lease time.Duration, f DoWork) error {
	if err := DoWithRenewAll(ctx, c, []*entroq.Task{task}, lease, func(ctx context.Context, finalize FinalizeRenewAll) error {
		// Call the single-task user function.
		if err := f(ctx, func() *entroq.Task { return finalize()[0] }); err != nil {
			return fmt.Errorf("do one: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("renew: %w", err)
	}
	return nil
}
