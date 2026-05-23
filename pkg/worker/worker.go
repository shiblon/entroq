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
//			WithDoModify(func(ctx context.Context, task *entroq.Task, value json.RawMessage, docs []*entroq.Doc) ([]entroq.ModifyArg, error) {
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
	"sort"
	"sync"
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

// Handler[T] is an interface that can be implemented to define work to be done.
// The value T is the pre-unmarshaled task value. Use T = json.RawMessage to
// receive raw bytes without any type-level unmarshaling.
//
// The three methods correspond to the three phases of task processing:
//   - TakeDocs: pre-work doc acquisition (optional; return nil to skip)
//   - DoWork: primary work, runs with background renewal
//   - Finish: commit phase, runs after renewal stops with stable task version
type Handler[T any] interface {
	// TakeDocs is called after a task is claimed and before DoWork. It declares which
	// docs the worker needs to claim ownership of before doing work. Return
	// nil to skip doc acquisition. A missing doc moves the task to the error
	// queue. A claimed (contended) required doc causes a retry.
	//
	// Note: task renewal begins as soon as the task is claimed, before TakeDocs
	// is called. Doc renewal (alongside the task) starts only once TakeDocs
	// returns and the docs are acquired. This means a very slow TakeDocs
	// implementation creates a window where the task is being renewed but claimed
	// docs are not yet. In natural use — where TakeDocs just returns a list of
	// DocClaim specs without doing I/O — this window is negligible.
	TakeDocs(context.Context, *entroq.Task, T) ([]*entroq.DocClaim, error)

	// DoWork is called by Worker.Run for each claimed task. The task is renewed in
	// the background while this function runs. value holds the result of
	// unmarshaling task.Value into T. Docs holds any docs acquired by TakeDocs; it
	// is non-nil but empty when no docs were acquired.
	//
	// On nil return, renewal is stopped and Finish (if set) is called with
	// the stable task version.
	//
	// On RetryError or MoveError, the task is retried or moved and Finish
	// is skipped. In both cases, the task's availability is set in the future
	// and its attempt count is incremented (these errors, while convenient for
	// managing task movement, are still errors).
	//
	// On any other error, Finish is skipped and the worker exits. Backoff and
	// restart are the responsibility of the process orchestrator (e.g.
	// Kubernetes, systemd). To retry or quarantine the task instead, return an
	// error wrapping RetryError or MoveError explicitly.
	DoWork(context.Context, *entroq.Task, T, []*entroq.Doc) error

	// Finish is called after DoWork returns nil and renewal has stopped. It
	// receives the stable (final renewed) task version, the same value passed
	// to DoWork, and the same docs. Use it to apply task modifications --
	// deletion, requeueing, doc changes, etc. Finish is skipped when DoWork
	// returns a non-nil error of any kind.
	Finish(context.Context, *entroq.Task, T, []*entroq.Doc) error
}

// MakeHandler defines a function that can be called to make a new handler.
// If you want to specify a full Handler[T] with your own state management,
// etc., then this is how you instruct the worker to create it in each
// invocation of Run.
type MakeHandler[T any] func() (Handler[T], error)

// DoModifyRun[T] is a function type that allows a work handler to be defined
// that passes modifications out instead of making those modifications itself
// in a Finish function. The docs parameter carries any docs claimed by
// WithTakeDocs, and can be empty.
//
// Return nil on success. To retry the task wrap RetryError; to quarantine it
// wrap MoveError. Any other non-nil error causes the worker to exit — backoff
// and restart are the responsibility of the process orchestrator.
type DoModifyRun[T any] func(context.Context, *entroq.Task, T, []*entroq.Doc) ([]entroq.ModifyArg, error)

// TakeRun[T] is a function that inspects a newly claimed task and
// declares what resources the worker needs before doing work. Returning a nil
// *ResourceRequest (or not setting WithTakeDocs) skips the acquisition phase.
//
// Note that if you want to specify multiple document claims (multiple primary
// keys, essentially), you can get into a situation where you fail to claim
// them all, leaving those whose claim succeeded in a waiting state until the
// lease expires.
//
// The proper recipe for taking documents safely is to claim them only if they
// must be claimed in order to carry out the task referenced in the parameters.
// Then it makes sense to hold a full exclusive lock on them all.
type TakeRun[T any] func(context.Context, *entroq.Task, T) ([]*entroq.DocClaim, error)

// DoFinishRun[T] defines a function shape that is called during the work and
// finish portions of task handling. In both cases, you are given a task, a
// value of that task properly typed, and if relevant, a document response with
// all required and zero or more optional documents.
//
// In the finish phase, the same information is available but all objects will
// have current version numbers for safe modification.
type DoFinishRun[T any] func(context.Context, *entroq.Task, T, []*entroq.Doc) error

// funcHandler[T] is a Handler[T] backed by plain functions.
type funcHandler[T any] struct {
	take   func(context.Context, *entroq.Task, T) ([]*entroq.DocClaim, error)
	do     func(context.Context, *entroq.Task, T, []*entroq.Doc) error
	finish func(context.Context, *entroq.Task, T, []*entroq.Doc) error
}

// TakeDocs runs the specified take function if set, otherwise returns nil.
func (h *funcHandler[T]) TakeDocs(ctx context.Context, task *entroq.Task, value T) ([]*entroq.DocClaim, error) {
	if h.take == nil {
		return nil, nil
	}
	return h.take(ctx, task, value)
}

// DoWork runs the specified "do" function.
func (h *funcHandler[T]) DoWork(ctx context.Context, task *entroq.Task, value T, docs []*entroq.Doc) error {
	if h.do == nil {
		return fmt.Errorf("no work function specified: %w", FatalError)
	}
	return h.do(ctx, task, value, docs)
}

// Finish runs the specified "finish" function if it has been defined.
func (h *funcHandler[T]) Finish(ctx context.Context, task *entroq.Task, value T, docs []*entroq.Doc) error {
	if h.finish == nil {
		return nil
	}
	return h.finish(ctx, task, value, docs)
}

// doModifyhandler is a special handler that keeps track of "desired
// modifications" passed out of the worker function. When work is specified in
// this way, modifications are not done by the implementer of the work
// function, rather they are "requested" by returning them. The worker then
// takes the responsibility of fixing up their versions to the latest claimed
// versions before packaging and sending the modification along. It's quite
// convenient, so it's the most common way to define work, but it requires a
// little state handling to pass requested modifications to the finish function.
type doModifyHandler[T any] struct {
	eqc      *entroq.EntroQ
	take     TakeRun[T]
	doModify DoModifyRun[T]

	initialTask *entroq.Task
	modArgs     []entroq.ModifyArg
}

func (h *doModifyHandler[T]) TakeDocs(ctx context.Context, task *entroq.Task, val T) ([]*entroq.DocClaim, error) {
	h.initialTask = task
	if h.take == nil {
		return nil, nil
	}
	return h.take(ctx, task, val)
}

func (h *doModifyHandler[T]) DoWork(ctx context.Context, task *entroq.Task, val T, docs []*entroq.Doc) error {
	var err error
	if h.initialTask == nil {
		h.initialTask = task
	}
	if h.doModify == nil {
		return fmt.Errorf("No work function specified: %w", FatalError)
	}
	h.modArgs, err = h.doModify(ctx, task, val, docs)
	return err
}

func (h *doModifyHandler[T]) Finish(ctx context.Context, finalTask *entroq.Task, val T, _ []*entroq.Doc) error {
	defer func() {
		h.initialTask = nil
		h.modArgs = nil
	}()
	if h.initialTask == nil {
		return fmt.Errorf("unexpected nil initial task in doModify finish: %w", FatalError)
	}
	if len(h.modArgs) == 0 {
		// Nothing to do!
		return nil
	}

	modification := entroq.NewModification("", h.modArgs...)

	if h.initialTask.Version > finalTask.Version {
		return fmt.Errorf("task updated inside worker body, expected version <= %v, got %v", finalTask.Version, h.initialTask.Version)
	}

	// Fix up modification versions to reflect final refreshed state.
	for _, t := range modification.Changes {
		if t.ID == finalTask.ID {
			t.Version = finalTask.Version
		}
	}
	for _, t := range modification.Depends {
		if t.ID == finalTask.ID {
			t.Version = finalTask.Version
		}
	}
	for _, t := range modification.Deletes {
		if t.ID == finalTask.ID {
			t.Version = finalTask.Version
		}
	}
	// TODO: fix up documents, too

	if _, err := h.eqc.Modify(ctx, entroq.WithModification(modification)); err != nil {
		if _, ok := entroq.AsDependency(err); ok {
			log.Printf("Worker ack failed, throwing away: %v", err)
			return fmt.Errorf("worker doModify finish dependency: %w", err)
		}
		if entroq.IsCanceled(err) || entroq.IsTimeout(err) {
			log.Printf("Worker exiting cleanly instead of acking: %v", err)
			return fmt.Errorf("canceled doModify finish: %w", err)
		}
		return fmt.Errorf("worker doModify finish: %w", err)
	}
	return nil
}

// Worker[T] defines a looping protocol that processes tasks in a queue. It
// goes through a claim/unmarshal/work/finalize cycle, where the work section
// has background task auto-renewal happening to allow the worker to maintain
// ownership of the task while it does its job.
//
// The type parameter T is the Go type of the task value. The worker
// unmarshals task.Value into T before calling DoWork/Finish, so handlers
// always receive a ready-to-use value. Use T = json.RawMessage to opt out of
// typed unmarshaling and receive the raw bytes directly.
//
// The finalization phase stops the renewal, freezes the task version, and
// allows the task to be deleted or modified safely.
//
// If WithTakeDocs is set, a resource acquisition phase runs between
// claiming the task and starting work. See WithTakeDocs for details.
type Worker[T any] struct {
	eqc *entroq.EntroQ

	errQMap ErrQMap

	// Creates a new handler. Called once per Run.
	makeHandler MakeHandler[T]
}

// workerOpts holds built-up worker options to be later checked against as a
// new worker is created.
type workerOpts[T any] struct {
	makeHandler MakeHandler[T]

	// These are all potential inputs to create a MakeHandler.
	take     TakeRun[T]
	doModify DoModifyRun[T]
	do       DoFinishRun[T]
	finish   DoFinishRun[T]

	errQMap ErrQMap
}

// New creates a new Worker[T] that claims tasks from its configured queues and
// presents pre-unmarshaled values of type T to the work handler.
//
// Options should be presented to, at a minimum, define the work to be done
// when a task is acquired. At least one of WithDoWork or WithDoModify should be
// specified, or WithMakeHandler if you have advanced needs.
func New[T any](eq *entroq.EntroQ, opts ...Option[T]) *Worker[T] {
	wOpts := new(workerOpts[T])
	for _, opt := range opts {
		opt(wOpts)
	}

	worker := &Worker[T]{
		eqc:         eq,
		errQMap:     wOpts.errQMap,
		makeHandler: wOpts.makeHandler,
	}

	if worker.makeHandler != nil {
		return worker
	}

	// No makeHandler specified, build one from what we have.
	// DoModify handlers win. TakeDocs is always used.
	if wOpts.doModify != nil {
		worker.makeHandler = func() (Handler[T], error) {
			return &doModifyHandler[T]{
				eqc:      eq,
				take:     wOpts.take,
				doModify: wOpts.doModify,
			}, nil
		}
	} else {
		worker.makeHandler = func() (Handler[T], error) {
			return &funcHandler[T]{
				take:   wOpts.take,
				do:     wOpts.do,
				finish: wOpts.finish,
			}, nil
		}
	}
	return worker
}

// Option[T] can be passed to New to modify worker parameters.
type Option[T any] func(*workerOpts[T])

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

// WithDoWork sets the primary work function for a worker. Overwrites any
// previous handler configuration.
func WithDoWork[T any](f DoFinishRun[T]) Option[T] {
	return func(wo *workerOpts[T]) {
		wo.do = f
	}
}

// WithFinish sets the finalization function for a worker, called after DoWork
// completes successfully and renewal has stopped. The function receives the
// stable (finally-renewed) task, the original unmarshaled value, and any docs
// acquired by WithTakeDocs. Overwrites any previous handler configuration.
func WithFinish[T any](f DoFinishRun[T]) Option[T] {
	return func(wo *workerOpts[T]) {
		wo.finish = f
	}
}

// WithDoModify sets a combined work and modification function that returns
// the list of modifications to apply after work is complete. Per-task state
// is stack-allocated in each pass through the worker loop, so concurrent Run
// goroutines are safe. Overwrites any previous configuration.
func WithDoModify[T any](f DoModifyRun[T]) Option[T] {
	return func(wo *workerOpts[T]) {
		wo.doModify = f
	}
}

// WithTakeDocs sets the doc acquisition function. Before work begins, this
// function is called with the claimed task to declare which docs are needed.
// Required docs that are missing cause the task to be treated as a poison pill
// (moved to the error queue). Required or optional docs claimed by another
// worker cause a backoff-and-retry. Read-only docs are fetched at acquisition
// time and version-pinned in the final Modify.
//
// When used with WithHandler, the handler's TakeDocs method takes
// precedence and WithTakeDocs has no effect.
func WithTakeDocs[T any](f TakeRun[T]) Option[T] {
	return func(wo *workerOpts[T]) {
		wo.take = f
	}
}

// WithMakeHandler sets a "new" function to create a handler.
// Why use this instead of just setting a handler? If you are going to call Run in
// multiple goroutines, your handler will be shared between them. Stateful
// handlers will not work properly. Here you can specify a function that
// returns a new handler when called, side-stepping that issue by ensuring that
// every call to Run has its own handler instance. The other pattern you can
// use is to specify functions to call (taking care to keep state local!):
//
// Always available:
//   - WithTakeDocs - specifies how to identify documents for a particular task.
//
// Two approaches to defining work/finishing:
//   - WithDoModify - a single function that does work, then returns desired modifications to be handled by the worker.
//   - WithDoWork, WithFinish - two functions to specify work, then to do modifications.
//
// It is expected that these single-function options are more ergonmic than
// this, but they are not suitable if your handler needs to manage state that
// is not captured in function parameters or otherwise concurrency-friendly.
func WithMakeHandler[T any](h MakeHandler[T]) Option[T] {
	return func(w *workerOpts[T]) {
		w.makeHandler = h
	}
}

// WithErrQMap sets the error queue mapping function for a worker.
func WithErrQMap[T any](f ErrQMap) Option[T] {
	return func(w *workerOpts[T]) {
		w.errQMap = f
	}
}

func (w *Worker[T]) handleSentinelErrors(ctx context.Context, sentinel error, task *entroq.Task, errQ string, opts *runOpt) (isSentinel bool, err error) {
	if errors.Is(sentinel, RetryError) {
		_, err := w.eqc.Modify(ctx, task.RetryOrQuarantine(sentinel.Error(), errQ, opts.maxAttempts, entroq.ArrivalTimeBy(opts.baseRetryDelay)))
		if err != nil {
			return true, fmt.Errorf("retry or quarantine modify: %w", err)
		}
		return true, nil
	}
	if errors.Is(sentinel, MoveError) {
		_, err := w.eqc.Modify(ctx, task.Quarantine(sentinel.Error(), errQ))
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

// acquireDocs performs the doc acquisition phase for a claimed task.
// It calls the provided take function to learn what is needed, then claims
// ownership of those documents.
//
// Returns a *entroq.DependencyError if claiming failed; the caller inspects
// HasMissingDocs vs HasClaimedDocs to decide whether to retry or move the
// task to the error queue.
func acquireDocs[T any](ctx context.Context, eqc *entroq.EntroQ, task *entroq.Task, value T, lease time.Duration, take TakeRun[T]) ([]*entroq.Doc, error) {
	if take == nil {
		return nil, nil
	}
	req, err := take(ctx, task, value)
	if err != nil {
		return nil, fmt.Errorf("take docs: %w", err)
	}
	if req == nil {
		return nil, nil
	}

	// Sort to avoid livelock from dining philosophers.
	sort.Slice(req, func(i, j int) bool {
		if req[i].Namespace != req[j].Namespace {
			return req[i].Namespace < req[j].Namespace
		}
		return req[i].Key < req[j].Key
	})

	var acquired []*entroq.Doc

	for _, cq := range req {
		cq.Duration = lease
		results, err := eqc.ClaimDocs(ctx, cq)
		if err != nil {
			return nil, err // caller inspects DependencyError
		}
		acquired = append(acquired, results...)
	}

	return acquired, nil
}

// runOne claims one task, unmarshals its value into T, runs the work function
// with renewal, and applies any resulting modification.
func (w *Worker[T]) runOne(ctx context.Context, handler Handler[T], opts *runOpt) error {
	// Note: do NOT cancel rCtx from inside the work function. If rCtx is
	// canceled while a renewal Modify is in flight over gRPC, the client sees
	// context.Canceled but the server may have already committed the renewal.
	// The stopRenew/taskCh handoff in DoWhileRenewing is the correct mechanism.
	rCtx, rCancel := context.WithCancel(ctx)
	defer rCancel()

	// Phase 1: Claim task and unmarshal its value.
	task, err := w.eqc.Claim(rCtx, entroq.From(opts.qs...), entroq.ClaimFor(opts.lease))
	if err != nil {
		return fmt.Errorf("worker (%q) claim: %w", opts.qs, err)
	}
	value, err := entroq.GetValue[T](task)
	if err != nil {
		return fmt.Errorf("worker (%q) unmarshal: %w", opts.qs, err)
	}

	// Phase 2: Acquire docs before renewal starts. Doc claims are sorted by
	// (namespace, key) to prevent dining-philosopher livelock when multiple
	// doc groups are acquired.
	docs, err := acquireDocs(rCtx, w.eqc, task, value, opts.lease, handler.TakeDocs)
	if err != nil {
		if de, ok := entroq.AsDependency(err); ok {
			var sentinelErr error
			if de.HasMissingDocs() {
				sentinelErr = fmt.Errorf("required doc missing: %w", MoveError)
			} else {
				sentinelErr = fmt.Errorf("doc contention: %w", RetryError)
			}
			errQ := w.ErrorQueueFor(task.Queue)
			if _, herr := w.handleSentinelErrors(ctx, sentinelErr, task, errQ, opts); herr != nil {
				return fmt.Errorf("handle sentinel error: %w", herr)
			}
			return nil
		}
		return fmt.Errorf("acquire docs: %w", err)
	}

	// Phase 3: DoWork with background renewal of task + docs together.
	var (
		sentinelErr error
		finalTask   *entroq.Task
		finalDocs   []*entroq.Doc
	)

	handleErr := DoWhileRenewing(rCtx, w.eqc,
		func(ctx context.Context, stop FinalizeRenew) error {
			defer func() {
				stable := stop()
				if len(stable.Tasks) > 0 {
					finalTask = stable.Tasks[0]
				}
				finalDocs = stable.Docs
			}()
			if err := handler.DoWork(ctx, task, value, docs); err != nil {
				if !isSentinelError(err) {
					return fmt.Errorf("task do: %w", err)
				}
				sentinelErr = err
			}
			return nil
		},
		entroq.RenewingTask(task),
		entroq.RenewingDocs(docs),
		entroq.WithRenewInterval(opts.lease),
	)

	if sentinelErr != nil {
		errQ := w.ErrorQueueFor(task.Queue)
		if _, err := w.handleSentinelErrors(ctx, sentinelErr, finalTask, errQ, opts); err != nil {
			return fmt.Errorf("handle sentinel error: %w", err)
		}
		return nil
	}

	if handleErr != nil {
		return fmt.Errorf("worker (%q): %w", opts.qs, handleErr)
	}

	// Phase 4: Finish with stable versions — renewal has stopped.
	if err := handler.Finish(ctx, finalTask, value, finalDocs); err != nil {
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

func isSentinelError(sentinel error) bool {
	return errors.Is(sentinel, RetryError) || errors.Is(sentinel, MoveError) || errors.Is(sentinel, FatalError)
}

// Run claims tasks from the worker queues and processes them in a loop until
// its context is canceled or an unrecoverable error is encountered.
func (w *Worker[T]) Run(ctx context.Context, opts ...RunOption) error {
	ro := &runOpt{
		lease:          entroq.DefaultClaimDuration,
		baseRetryDelay: DefaultRetryDelay,
	}
	for _, opt := range opts {
		opt(ro)
	}

	if len(ro.qs) == 0 {
		return fmt.Errorf("no queues specified to work on")
	}
	handler, err := w.makeHandler()
	if err != nil {
		return fmt.Errorf("failed to make handler: %v: %w", err, FatalError)
	}

	for {
		if err := w.runOne(ctx, handler, ro); err != nil {
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

// RetryErrorf is a convenience wrapper for making a retry error for workers.
// Workers will retry a task with this error up to a threshold, then quarantine
// it.
func RetryErrorf(fstr string, args ...any) error {
	return fmt.Errorf(fstr+": %w", append(args, RetryError)...)
}

// MoveErrorf is a convenience wrapper for making a move error for workers.
// These are errors with a task that is not going to do better with a retry
// (malformed, for example).
func MoveErrorf(fstr string, args ...any) error {
	return fmt.Errorf(fstr+": %w", append(args, MoveError)...)
}

// FatalErrorf is a convenience wrapper for making a fatal error for workers.
// These are errors the worker is not expected to recover from, so it should
// crash.
func FatalErrorf(fstr string, args ...any) error {
	return fmt.Errorf(fstr+": %w", append(args, FatalError)...)
}

// Renewal Machinery

// FinalizeRenew defines a function that can be called to stop renewal from a
// worker routine. It returns a RenewResponse with tasks and/or docs with
// stable versions (no longer renewing).
type FinalizeRenew func() *entroq.RenewResponse

// DoWork defines a function that handles tasks and docs.
type DoWork func(ctx context.Context, stop FinalizeRenew) error

// DoWhileRenewing runs the given work function while keeping the provided tasks and docs claimed in the background.
func DoWhileRenewing(ctx context.Context, c *entroq.EntroQ, doWork DoWork, opts ...entroq.RenewOption) error {
	conf := entroq.NewRenewConfig(opts...)
	if conf.IsEmpty() {
		return fmt.Errorf("do while renewing: nothing to renew")
	}
	type outVal struct {
		tasks []*entroq.Task
		docs  []*entroq.Doc
		err   error
	}
	taskCh := make(chan outVal, 1)

	g, ctx := errgroup.WithContext(ctx)

	fctx, fcancel := context.WithCancelCause(ctx)
	defer fcancel(nil)

	stopRenew := make(chan struct{})
	g.Go(func() error {
		renewed := conf.Tasks
		renewedDocs := conf.Docs
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
			case <-time.After(conf.Interval / 2):
				if stopErr != nil {
					break
				}
				resp, err := c.Renew(ctx,
					entroq.RenewingTasks(renewed),
					entroq.RenewingDocs(renewedDocs),
					entroq.WithRenewInterval(conf.Interval))
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
				renewed = resp.Tasks
				renewedDocs = resp.Docs
			case out <- outVal{renewed, renewedDocs, stopErr}:
				return nil
			}
		}
	})

	// finalize is safe to call any number of times from any goroutine.
	// sync.Once ensures stopRenew is closed exactly once and taskCh is read
	// exactly once; subsequent calls return the already-captured response.
	var (
		finalizeOnce sync.Once
		finalResp    *entroq.RenewResponse
	)
	finalize := func() *entroq.RenewResponse {
		finalizeOnce.Do(func() {
			close(stopRenew)
			out := <-taskCh
			if out.err != nil {
				fcancel(out.err)
			}
			finalResp = &entroq.RenewResponse{
				Tasks: out.tasks,
				Docs:  out.docs,
			}
		})
		return finalResp
	}

	g.Go(func() error {
		if err := doWork(fctx, finalize); err != nil {
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

