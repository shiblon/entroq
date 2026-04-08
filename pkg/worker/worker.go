package worker

import (
	"context"
	"encoding/json"
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

// Handler[T] is an interface that can be implemented to define work to be done.
// The value T is the pre-unmarshaled task value. Use T = json.RawMessage to
// receive raw bytes without any type-level unmarshaling.
type Handler[T any] interface {
	// TaskDo is called by Worker.Run for each claimed task. The task is renewed
	// in the background while this function runs. value holds the result of
	// unmarshaling task.Value into T.
	//
	// On nil return, renewal is stopped and TaskFinish (if set) is called with
	// the stable task version.
	//
	// On RetryError or MoveError, the task is retried or moved respectively and
	// TaskFinish is skipped.
	//
	// On any other error, TaskFinish is skipped and the error is treated as a
	// fundamental error subject to backoff.
	TaskDo(context.Context, *entroq.Task, T) error

	// TaskFinish is called after TaskDo returns nil and renewal has stopped.
	// It receives the stable (final renewed) task version and the same value
	// that was passed to TaskDo. Use it to apply task modifications --
	// deletion, requeueing, etc. -- and any associated cleanup. TaskFinish is
	// skipped when TaskDo returns a non-nil error.
	TaskFinish(context.Context, *entroq.Task, T) error
}

// DoModifyRun[T] is a function type that allows a work handler to be defined
// that passes modifications out instead of making those modifications itself.
// It's a convenience for callers that don't need special finalization.
type DoModifyRun[T any] func(context.Context, *entroq.Task, T) ([]entroq.ModifyArg, error)

// funcHandler[T] is a Handler[T] backed by plain functions.
type funcHandler[T any] struct {
	do     func(context.Context, *entroq.Task, T) error
	finish func(context.Context, *entroq.Task, T) error
}

// TaskDo runs the specified "do" function.
func (h *funcHandler[T]) TaskDo(ctx context.Context, task *entroq.Task, value T) error {
	if h.do == nil {
		return fmt.Errorf("no work function specified")
	}
	return h.do(ctx, task, value)
}

// TaskFinish runs the specified "finish" function if it has been defined.
func (h *funcHandler[T]) TaskFinish(ctx context.Context, task *entroq.Task, value T) error {
	if h.finish == nil {
		return nil
	}
	return h.finish(ctx, task, value)
}

// Worker[T] defines a looping protocol that processes tasks in a queue. It
// goes through a claim/unmarshal/work/finalize cycle, where the work section
// has background task auto-renewal happening to allow the worker to maintain
// ownership of the task while it does its job.
//
// The type parameter T is the Go type of the task value. The worker
// unmarshals task.Value into T before calling TaskDo/TaskFinish, so handlers
// always receive a ready-to-use value. Use T = json.RawMessage to opt out of
// typed unmarshaling and receive the raw bytes directly.
//
// The finalization phase stops the renewal, freezes the task version, and
// allows the task to be deleted or modified safely.
type Worker[T any] struct {
	eqc *entroq.EntroQ

	lease          time.Duration
	baseRetryDelay time.Duration
	backoff        time.Duration

	handler  Handler[T]
	doModify DoModifyRun[T] // set by WithDoModify; per-task state lives in runOne

	// ErrQMap maps an inbox to the queue tasks are moved to if a MoveError
	// is returned from a worker's run function, or if the task value cannot
	// be decoded into T.
	ErrQMap ErrQMap

	// MaxAttempts indicates how many attempts are too many before a retryable
	// error becomes permanent and the task is moved to an error queue.
	MaxAttempts int32
}

// New creates a new Worker[T] that claims tasks from the given queues and
// presents pre-unmarshaled values of type T to the work handler. Configure
// the worker with options like WithDo, WithFinish, or WithDoModify.
//
// Use T = json.RawMessage for untyped operation (raw JSON bytes passed
// through as-is).
func New[T any](eq *entroq.EntroQ, opts ...Option[T]) *Worker[T] {
	w := &Worker[T]{
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

// Option[T] can be passed to New to modify worker parameters.
type Option[T any] func(*Worker[T])

// WithDo sets the primary work function for a worker. The function receives
// the claimed task and its value pre-unmarshaled into T. Overwrites any
// previous handler configuration.
func WithDo[T any](f func(context.Context, *entroq.Task, T) error) Option[T] {
	return func(w *Worker[T]) {
		switch fh := w.handler.(type) {
		case *funcHandler[T]:
			fh.do = f
		default:
			w.handler = &funcHandler[T]{do: f}
		}
	}
}

// WithFinish sets the finalization function for a worker, called after Do
// completes successfully and renewal has stopped. The function receives the
// stable (finally-renewed) task and the original unmarshaled value. Overwrites
// any previous handler configuration.
func WithFinish[T any](f func(context.Context, *entroq.Task, T) error) Option[T] {
	return func(w *Worker[T]) {
		if fh, ok := w.handler.(*funcHandler[T]); ok {
			fh.finish = f
		} else {
			w.handler = &funcHandler[T]{finish: f}
		}
	}
}

// WithDoModify sets a combined work and modification function that returns
// the list of modifications to apply after work is complete. Per-task state
// is stack-allocated in each runOne call, so concurrent Run goroutines are
// safe. Overwrites any previous configuration.
func WithDoModify[T any](f DoModifyRun[T]) Option[T] {
	return func(w *Worker[T]) {
		w.handler = nil
		w.doModify = f
	}
}

// WithHandler sets a custom task handler for a worker.
func WithHandler[T any](h Handler[T]) Option[T] {
	return func(w *Worker[T]) {
		w.handler = h
	}
}

// WithLease sets the frequency of task renewal.
func WithLease[T any](d time.Duration) Option[T] {
	return func(w *Worker[T]) {
		w.lease = d
	}
}

// WithErrQMap sets a function that maps from inbox queue names to error queue
// names. Defaults to DefaultErrQMap.
func WithErrQMap[T any](f ErrQMap) Option[T] {
	return func(w *Worker[T]) {
		w.ErrQMap = f
	}
}

// WithMaxAttempts sets the maximum attempts allowed before a RetryError turns
// into a MoveError. If 0 (the default), there is no maximum.
func WithMaxAttempts[T any](m int32) Option[T] {
	return func(w *Worker[T]) {
		w.MaxAttempts = m
	}
}

// WithBaseRetryDelay sets the base delay for a retried task.
func WithBaseRetryDelay[T any](d time.Duration) Option[T] {
	return func(w *Worker[T]) {
		w.baseRetryDelay = d
	}
}

// WithBackoff sets how long the worker sleeps after an infrastructure error
// before attempting to claim again. Defaults to DefaultBackoff.
func WithBackoff[T any](d time.Duration) Option[T] {
	return func(w *Worker[T]) {
		w.backoff = d
	}
}

// DefaultErrQMap appends "/err" to the inbox.
func DefaultErrQMap(inbox string) string {
	return inbox + "/err"
}

// runOne claims one task, unmarshals its value into T, runs the work function
// with renewal, and applies any resulting modification. All per-task state is
// local to this call, making concurrent Run goroutines safe.
func (w *Worker[T]) runOne(ctx context.Context, qs []string) error {
	task, err := w.eqc.Claim(ctx, entroq.From(qs...), entroq.ClaimFor(w.lease))
	if err != nil {
		if entroq.IsCanceled(err) {
			return err
		}
		log.Printf("Worker claim failed (%q), backing off %v: %v", qs, w.backoff, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(w.backoff):
		}
		return nil
	}

	errQ := w.ErrQMap(task.Queue)

	// Decode task.Value into T. When T is json.RawMessage, skip unmarshaling
	// entirely and alias the slice directly -- no copy, no allocation. This is
	// the fast path for callers that want raw bytes without typed decoding.
	// Note: value and task.Value share the same backing array in that case, so
	// in-place byte mutation of either would affect both. Appending is safe.
	var value T
	if task.Value != nil {
		switch raw := any(&value).(type) {
		case *json.RawMessage:
			*raw = task.Value
		default:
			if err := json.Unmarshal(task.Value, &value); err != nil {
				log.Printf("Worker decode error, moving task %v to %q: %v", task.ID, errQ, err)
				if _, _, merr := w.eqc.Modify(ctx, task.Quarantine(err.Error(), errQ)); merr != nil {
					log.Printf("Worker failed to quarantine malformed task %v: %v", task.ID, merr)
				}
				return nil
			}
		}
	}

	// Build a per-invocation handler. For doModify, state lives here on the
	// stack so concurrent Run goroutines never share mutable handler fields.
	handler := w.handler
	if w.doModify != nil {
		var mods []entroq.ModifyArg
		initial := task
		handler = &funcHandler[T]{
			do: func(ctx context.Context, task *entroq.Task, value T) error {
				var err error
				mods, err = w.doModify(ctx, task, value)
				return err
			},
			finish: func(ctx context.Context, renewed *entroq.Task, _ T) error {
				modification := entroq.NewModification("", mods...)
				switch {
				case initial.Version > renewed.Version:
					return fmt.Errorf("task updated inside worker body, expected version <= %v, got %v", renewed.Version, initial.Version)
				case initial.Version < renewed.Version:
					for _, t := range modification.Changes {
						if t.ID == renewed.ID {
							t.Version = renewed.Version
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
				if _, _, err := w.eqc.Modify(ctx, entroq.WithModification(modification)); err != nil {
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
			},
		}
	}

	var stable *entroq.Task
	var handleErr error

	if err := DoWithRenew(ctx, w.eqc, task, w.lease, func(ctx context.Context, stop FinalizeRenew) error {
		defer func() { stable = stop() }()
		if err := handler.TaskDo(ctx, task, value); err != nil {
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
		return err
	}

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

	if err := handler.TaskFinish(ctx, stable, value); err != nil {
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

// Run claims tasks from the worker queues and processes them in a loop until
// its context is canceled or an unrecoverable error is encountered.
// You may call Run multiple times on the same worker.
func (w *Worker[T]) Run(ctx context.Context, qs ...string) error {
	if len(qs) == 0 {
		return fmt.Errorf("no queues specified to work on")
	}

	for {
		if err := w.runOne(ctx, qs); err != nil {
			if entroq.IsCanceled(err) || entroq.IsTimeout(err) {
				log.Printf("worker was asked to quit: %v", ctx.Err())
				return nil
			}
			return fmt.Errorf("worker (%q): %w", qs, err)
		}
	}
}

// MoveError causes a task to be moved to a specified queue.
type MoveError struct {
	Err error
}

// NewMoveError creates a new MoveError from the given error.
func NewMoveError(err error) *MoveError {
	return &MoveError{Err: err}
}

// MoveErrorf creates a MoveError given a format string and values.
func MoveErrorf(format string, values ...any) *MoveError {
	return NewMoveError(fmt.Errorf(format, values...))
}

func (e *MoveError) Error() string { return e.Err.Error() }
func (e *MoveError) Unwrap() error { return e.Err }

// RetryError causes a task to be retried, incrementing its Attempt field.
// If MaxAttempts is positive and nonzero, and has been reached, then this
// behaves the same as a MoveError.
type RetryError struct {
	Err error
}

// NewRetryError creates a new RetryError from the given error.
func NewRetryError(err error) *RetryError {
	return &RetryError{Err: err}
}

// RetryErrorf creates a RetryError in the same way you would create an error
// with fmt.Errorf.
func RetryErrorf(format string, values ...any) *RetryError {
	return NewRetryError(fmt.Errorf(format, values...))
}

func (e *RetryError) Error() string { return e.Err.Error() }
func (e *RetryError) Unwrap() error { return e.Err }

// Renewal Machinery

// FinalizeRenewAll defines a function that can be called to stop renewal from
// a worker routine. It returns a slice of tasks that are no longer being
// renewed, so versions are stable.
type FinalizeRenewAll func() []*entroq.Task

// FinalizeRenew defines a function that can be called to stop renewal from a
// worker routine. It returns a single task that is no longer being renewed, so
// its version is stable.
type FinalizeRenew func() *entroq.Task

// DoWorkAll defines a function that accepts a 'stop' function so that work
// can be done, renewal can be stopped to get stable task versions, and cleanup
// can happen. For multiple tasks.
type DoWorkAll func(ctx context.Context, stop FinalizeRenewAll) error

// DoWork defines a function like WorkAll, but handles only one task.
type DoWork func(ctx context.Context, stop FinalizeRenew) error

// DoWithRenewAll runs the provided function while keeping all given task
// leases renewed.
func DoWithRenewAll(ctx context.Context, c *entroq.EntroQ, tasks []*entroq.Task, lease time.Duration, f DoWorkAll) error {
	type outVal struct {
		tasks []*entroq.Task
		err   error
	}
	taskCh := make(chan outVal, 1)

	g, ctx := errgroup.WithContext(ctx)

	fctx, fcancel := context.WithCancelCause(ctx)
	defer fcancel(nil)

	stopRenew := make(chan struct{})
	g.Go(func() error {
		renewed := tasks
		var out chan<- outVal
		var stopErr error
		doneCh := ctx.Done()
		for {
			select {
			case <-stopRenew:
				out = taskCh
				stopRenew = nil
			case <-doneCh:
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
						fcancel(depErr)
						stopErr = depErr
						out = taskCh
						break
					}
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

// DoWithRenew runs the provided function while keeping the given task lease
// renewed.
func DoWithRenew(ctx context.Context, c *entroq.EntroQ, task *entroq.Task, lease time.Duration, f DoWork) error {
	if err := DoWithRenewAll(ctx, c, []*entroq.Task{task}, lease, func(ctx context.Context, finalize FinalizeRenewAll) error {
		if err := f(ctx, func() *entroq.Task { return finalize()[0] }); err != nil {
			return fmt.Errorf("do one: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("renew: %w", err)
	}
	return nil
}
