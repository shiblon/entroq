// Package worker provides a high-level looping protocol for processing tasks.
//
// It handles the "Claim -> Work -> Renew -> Modify" lifecycle, ensuring that:
// 1. Tasks are renewed in the background while work is ongoing.
// 2. Renewal stops before finalization to ensure a stable task version.
// 3. Failures are handled through retry or quarantine to an error queue.
// 4. Concurrency is safe and easy to manage via context cancellation.
//
// # Quick Start
//
// A worker is typically created with a set of options that define its behavior.
// Below is a minimal example using a "DoModify" pattern, which accepts a single
// function to run, and that function does work and returns modifications it
// wants to make:
//
//		client, _ := entroq.New(ctx, mem.Opener()) // Open an in-memory EntroQ backend.
//		if err := worker.Run(ctx,
//	 		WithQueues("/my/inbox"),
//			WithDoModify(func(ctx context.Context, task *entroq.Task, value json.RawMessage) ([]entroq.ModifyArg, error) {
//		    	log.Printf("Working on task %v", task.ID)
//		    	return []entroq.ModifyArg{task.Delete()}, nil
//			}),
//		); err != nil {
//		    log.Fatalf("Worker failed: %v", err)
//		}
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
// worker task errors out as retryable. This is an exponential backoff baseline.
const DefaultRetryDelay = 30 * time.Second

// DefaultBackoff is the time a worker sleeps after a fundamental error
// (renewal failure, connection error, etc.) before attempting to claim again.
// It prevents "thundering herd" or "tight loop" failures during outages.
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

	errQMap ErrQMap

	handler  Handler[T]
	doModify DoModifyRun[T] // set by WithDoModify; per-task state lives in runOne
}

// New creates a new Worker[T] that claims tasks from its configured queues and
// presents pre-unmarshaled values of type T to the work handler.
func New[T any](eq *entroq.EntroQ, opts ...Option[T]) *Worker[T] {
	w := &Worker[T]{
		eqc: eq,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Option[T] can be passed to New to modify worker parameters.
type Option[T any] func(*Worker[T])

// ErrorQueueFor returns the error queue for the given inbox, using the worker's
// configured mapping or the default if none is set.
func (w *Worker[T]) ErrorQueueFor(inbox string) string {
	if w.errQMap != nil {
		return w.errQMap(inbox)
	}
	return DefaultErrQMap(inbox)
}

// DefaultErrQMap is the default error queue mapping function. It appends
// "/err" to the inbox name.
func DefaultErrQMap(inbox string) string {
	return inbox + "/err"
}

// WithDo sets the primary work function for a worker. Overwrites any
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
// is stack-allocated in each pass through the worker loop, so concurrent Run
// goroutines are safe. Overwrites any previous configuration.
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

// WithErrQMap sets the error queue mapping function for a worker.
func WithErrQMap[T any](f ErrQMap) Option[T] {
	return func(w *Worker[T]) {
		w.errQMap = f
	}
}

func (w *Worker[T]) handleSentinelErrors(ctx context.Context, sentinel error, task *entroq.Task, errQ string, opts *runOpt) (isSentinel bool, err error) {
	if errors.Is(sentinel, RetryError) {
		_, _, err := w.eqc.Modify(ctx, task.RetryOrQuarantine(sentinel.Error(), errQ, opts.maxAttempts, entroq.ArrivalTimeBy(opts.baseRetryDelay)))
		if err != nil {
			return true, fmt.Errorf("retry or quarantine modify: %w", err)
		}
		return true, nil
	}
	if errors.Is(sentinel, MoveError) {
		_, _, err := w.eqc.Modify(ctx, task.Quarantine(sentinel.Error(), errQ))
		if err != nil {
			return true, fmt.Errorf("quarantine modify: %w", err)
		}
		return true, nil
	}
	if errors.Is(sentinel, FatalError) {
		return true, sentinel
	}
	return false, nil
}

func renewModVersions(mod *entroq.Modification, renewed *entroq.TaskID) {
	for _, t := range mod.Changes {
		if t.ID == renewed.ID {
			t.Version = renewed.Version
		}
	}
	for _, t := range mod.Depends {
		if t.ID == renewed.ID {
			t.Version = renewed.Version
		}
	}
	for _, t := range mod.Deletes {
		if t.ID == renewed.ID {
			t.Version = renewed.Version
		}
	}
}

// runOne claims one task, unmarshals its value into T, runs the work function
// with renewal, and applies any resulting modification.
func (w *Worker[T]) runOne(ctx context.Context, opts *runOpt) error {
	// Set up the first (work) handler function, and track doModify args.
	var taskDo DoTaskWork[T]
	var returnedModArgs []entroq.ModifyArg // save these for later if it's a doModify handler
	if w.doModify == nil {
		taskDo = w.handler.TaskDo
	} else {
		taskDo = func(ctx context.Context, task *entroq.Task, value T) (err error) {
			returnedModArgs, err = w.doModify(ctx, task, value)
			return err
		}
	}

	// Run ClaimWithRenew, capture initial and final tasks, react to sentinel errors
	rCtx, rCancel := context.WithCancel(ctx)
	defer rCancel()
	var (
		initialTask  *entroq.Task
		initialValue T
		sentinelErr  error
	)
	finalTask, handleErr := ClaimWithRenew(rCtx, w.eqc, opts.qs, opts.lease, func(ctx context.Context, task *entroq.Task, value T) error {
		initialTask = task
		initialValue = value
		defer rCancel()
		if err := taskDo(ctx, task, value); err != nil {
			// Check for sentinel errors, pass them out if they are, otherwise
			// run it up the chain.
			if isSentinelError(err) {
				sentinelErr = err
				return nil // retry and move errors will be handled outside with the final task.
			}
			return fmt.Errorf("task do: %v", err)
		}
		return nil
	})

	// Sentinel errors are special - handle and exit.
	if sentinelErr != nil {
		// Figure out the error queue, handle sentinel errors.
		errQ := w.ErrorQueueFor(initialTask.Queue)
		if _, err := w.handleSentinelErrors(ctx, sentinelErr, finalTask, errQ, opts); err != nil {
			// Something went wrong trying to handle the error itself.
			// Includes cancellation and timeouts.
			return fmt.Errorf("handle sentinel error: %w", err)
		}
		return nil
	}

	if handleErr != nil {
		// Not a sentinel, institute backoff in case the worker jUst loops
		// really fast on a broken network connection or similar.
		log.Printf("Worker claim failed (%q), backing off %v: %v", opts.qs, opts.backoff, handleErr)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opts.backoff):
		}
		return fmt.Errorf("worker claim failed (%q), backed off %v: %w", opts.qs, opts.backoff, handleErr)
	}

	// Define the finalizer function, we'll call it below.
	var taskFinish DoTaskWork[T]
	if w.doModify == nil {
		taskFinish = w.handler.TaskFinish
	} else {
		taskFinish = func(ctx context.Context, task *entroq.Task, value T) error {
			modification := entroq.NewModification("", returnedModArgs...)
			switch {
			case initialTask.Version > finalTask.Version:
				return fmt.Errorf("task updated inside worker body, expected version <= %v, got %v", finalTask.Version, initialTask.Version)
			case initialTask.Version < finalTask.Version:
				renewModVersions(modification, finalTask.IDVersion())
			}
			if _, _, err := w.eqc.Modify(ctx, entroq.WithModification(modification)); err != nil {
				if _, ok := entroq.AsDependency(err); ok {
					log.Printf("Worker ack failed, throwing away: %v", err)
					return fmt.Errorf("worker dependency: %w", err)
				}
				if entroq.IsCanceled(err) || entroq.IsTimeout(err) {
					log.Printf("Worker exiting cleanly instead of acking: %v", err)
					return fmt.Errorf("canceled in compact finish: %w", err)
				}
				return fmt.Errorf("worker doModify finish: %w", err)
			}
			return nil
		}
	}

	// Finally call the finish function, handle any errors.
	if err := taskFinish(ctx, finalTask, initialValue); err != nil {
		// Too late to get sentinel errors - we don't know what task we have to
		// move or retry it.
		if de, ok := entroq.AsDependency(err); ok {
			if opts.onDepError != nil {
				if err := opts.onDepError(ctx, finalTask, de); err != nil {
					return fmt.Errorf("on dependency handler: %w", err)
				}
			}
			log.Printf("Worker finish failed (%q), throwing away: %v", opts.qs, de)
			return nil
		}
		if entroq.IsTimeout(err) || entroq.IsCanceled(err) {
			log.Printf("Worker exiting cleanly: %v", err)
			return fmt.Errorf("canceled in finish: %w", err)
		}
		return fmt.Errorf("worker finish (%q): %w", opts.qs, err)
	}
	return nil
}

// RunOption is an option for a run call.
type RunOption func(*runOpt)

type runOpt struct {
	qs             []string
	baseRetryDelay time.Duration
	maxAttempts    int32
	lease          time.Duration
	onDepError     func(context.Context, *entroq.Task, *entroq.DependencyError) error
	backoff        time.Duration
}

// Watching specifies the queues Run will watch.
func Watching(qs ...string) RunOption {
	return func(ro *runOpt) {
		ro.qs = qs
	}
}

// WithDependencyHandler specifies the function to call when a task dependency
// fails. Use to, for example, reload stale config tasks.
func WithDependencyHandler(f func(ctx context.Context, task *entroq.Task, de *entroq.DependencyError) error) RunOption {
	return func(ro *runOpt) {
		ro.onDepError = f
	}
}

// WithLease sets the frequency of task renewal.
func WithLease(d time.Duration) RunOption {
	return func(ro *runOpt) {
		ro.lease = d
	}
}

// WithMaxAttempts sets the maximum attempts allowed before a RetryError turns
// into a MoveError. If 0 (the default), there is no maximum.
func WithMaxAttempts(m int32) RunOption {
	return func(ro *runOpt) {
		ro.maxAttempts = m
	}
}

// WithBaseRetryDelay sets the base delay for a retried task.
func WithBaseRetryDelay(d time.Duration) RunOption {
	return func(ro *runOpt) {
		ro.baseRetryDelay = d
	}
}

// WithBackoff sets how long the worker sleeps after an infrastructure error
// before attempting to claim again. Defaults to DefaultBackoff.
func WithBackoff(d time.Duration) RunOption {
	return func(ro *runOpt) {
		ro.backoff = d
	}
}

func isSentinelError(sentinel error) bool {
	return errors.Is(sentinel, RetryError) || errors.Is(sentinel, MoveError) || errors.Is(sentinel, FatalError)
}

// Run claims tasks from the worker queues and processes them in a loop until
// its context is canceled or an unrecoverable error is encountered.
func (w *Worker[T]) Run(ctx context.Context, opts ...RunOption) error {
	ro := &runOpt{
		lease:          entroq.DefaultClaimDuration,
		baseRetryDelay: DefaultRetryDelay,
		backoff:        DefaultBackoff,
	}
	for _, opt := range opts {
		opt(ro)
	}

	if len(ro.qs) == 0 {
		return fmt.Errorf("no queues specified to work on")
	}

	if w.handler == nil && w.doModify == nil {
		return fmt.Errorf("no work handler specified")
	}

	for {
		if err := w.runOne(ctx, ro); err != nil {
			if entroq.IsCanceled(err) || entroq.IsTimeout(err) {
				log.Printf("worker was asked to quit: %v", ctx.Err())
				return nil
			}
			return fmt.Errorf("worker (%q): %w", ro.qs, err)
		}
	}
}

var (
	// RetryError can be returned from a worker to cause its claimed task to be
	// marked as attempted again, and to cause its At to be at a time in the
	// future. Convenient for work that fails due to likely transient causes.
	RetryError = errors.New("worker retry")

	// MoveError can be returned from a worker to cause its claimed task to
	// move, e.g., to a quarantine queue for inspection. Helpful if operating
	// on a task seems to have non-retriable errors, but the task is important.
	MoveError = errors.New("worker move")

	// FatalError can be returned from a worker to cause it to stop immediately.
	// This is useful if a task handler determines that the worker cannot or
	// should not continue processing tasks.
	FatalError = errors.New("worker fatal")
)

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

// DoTaskWork defines a function that can be called to do work on a task and its value.
// Used in ClaimWithRenew.
type DoTaskWork[T any] func(ctx context.Context, task *entroq.Task, value T) error

// ClaimWithRenew claims a task from the given queues (blocking)
func ClaimWithRenew[T any](ctx context.Context, eq *entroq.EntroQ, qs []string, lease time.Duration, f DoTaskWork[T]) (*entroq.Task, error) {
	task, err := eq.Claim(ctx, entroq.From(qs...), entroq.ClaimFor(lease))
	if err != nil {
		return nil, fmt.Errorf("claim with renew: %w", err)
	}

	value, err := entroq.GetValue[T](task)
	if err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	g.Go(func() error {
		if err := f(ctx, task, value); err != nil {
			return fmt.Errorf("worker (%q): %w", qs, err)
		}
		cancel()
		return nil
	})

	renewTask := task
	g.Go(func() error {
		for {
			select {
			case <-time.After(lease / 2):
				_, mods, err := eq.Modify(ctx, renewTask.Change(entroq.ArrivalTimeBy(lease)))
				if err != nil {
					return fmt.Errorf("renew for: %w", err)
				}
				renewTask = mods[0]
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})

	if err := g.Wait(); err != nil {
		if entroq.IsCanceled(err) || entroq.IsTimeout(err) {
			// Clean exit, return current task.
			return renewTask, nil
		}
		return nil, fmt.Errorf("worker stopped (%q): %w", qs, err)
	}
	// A successful renewal function *always* returns at least a context.Canceled
	// error, so we should never get here.
	return nil, fmt.Errorf("worker exited unexpectedly early (%q)", qs)
}

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
		if err := f(ctx, func() *entroq.Task {
			return finalize()[0]
		}); err != nil {
			return fmt.Errorf("do one: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("renew: %w", err)
	}
	return nil
}
