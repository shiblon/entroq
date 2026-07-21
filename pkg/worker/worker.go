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
// A worker is created with a client and a set of options, then run against one
// or more queues. Below is a minimal example using the "DoModify" pattern: a
// single function that does the work and returns a Result describing the
// modifications to apply.
//
//	client, _ := entroq.New(ctx, mem.Opener()) // Open an in-memory EntroQ backend.
//	w := worker.New[json.RawMessage](client,
//		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, value json.RawMessage, docs []*entroq.Doc) (*worker.Result, error) {
//			log.Printf("Working on task %v", task.ID)
//			return worker.Modify(task.Delete()), nil
//		}),
//	)
//	if err := w.Run(ctx, worker.Watching("/my/inbox")); err != nil {
//		log.Fatalf("Worker failed: %v", err)
//	}
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
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/errgroup"
)

// ErrQMap is a function that maps from an inbox name to its "move on error"
// error box name. If no mapping is found, a suitable default should be
// returned.
type ErrQMap func(inbox string) string

// DefaultRetryDelay is the amount by which to advance the arrival time when a
// worker task errors out as retryable. This is an exponential backoff baseline.
const DefaultRetryDelay = 30 * time.Second

// Modifier is the modification-capable subset of *entroq.EntroQ handed to a
// handler's Finish phase (and to WithFinish functions). Finish runs after
// renewal has stopped, so committing the task is safe, and committing is all it
// needs -- reads, claims, and renewals are neither required there nor offered.
// A handler that needs more implements Handler via WithMakeHandler and captures
// a full client.
type Modifier interface {
	Modify(ctx context.Context, modArgs ...entroq.ModifyArg) (*entroq.ModifyResponse, error)
}

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
	// Kubernetes, systemd). To retry or quarantine the task instead, return a
	// RetryError or MoveError (see RetryErrorf and MoveErrorf).
	DoWork(context.Context, *entroq.Task, T, []*entroq.Doc) error

	// Finish is called after DoWork returns nil and renewal has stopped. It
	// receives a Modifier (the worker's client, narrowed to modification), the
	// stable (final renewed) task version, the same value passed to DoWork, and
	// the same docs. Use it to apply task modifications -- deletion, requeueing,
	// doc changes, etc. Finish is skipped when DoWork returns a non-nil error of
	// any kind.
	//
	// Finish is the ONLY phase handed a client, and only a Modifier, by design:
	// it runs after renewal has stopped, so modifying the claimed task here is
	// safe, and committing is all it needs to do. TakeDocs and DoWork run under
	// background renewal, where mutating the claimed task would race the renewer,
	// so they are given no client. A handler that needs more than a commit here
	// (or a client in the earlier phases) implements Handler via WithMakeHandler
	// and captures a full client in a closure.
	Finish(context.Context, Modifier, *entroq.Task, T, []*entroq.Doc) error
}

// MakeHandler defines a function that can be called to make a new handler.
// If you want to specify a full Handler[T] with your own state management,
// etc., then this is how you instruct the worker to create it in each
// invocation of Run.
type MakeHandler[T any] func() (Handler[T], error)

// DoModifyRun[T] is the common worker pattern: do the work, then return a Result
// describing the modifications the worker should apply (and, optionally, work to
// run once it succeeds) rather than committing them yourself in a Finish
// function. It is not handed a client: it runs under renewal, and its output is
// the returned Result, which the worker commits in Finish at the stable version.
// The docs parameter carries any docs claimed by WithTakeDocs, and can be empty.
//
// Return the Result (built with Modify) and a nil error on success; a nil Result
// is a valid no-op. To retry the task return a RetryError (RetryErrorf); to move
// it return a MoveError (MoveErrorf). Any other non-nil error causes the worker
// to exit -- backoff and restart are the responsibility of the process
// orchestrator.
type DoModifyRun[T any] func(context.Context, *entroq.Task, T, []*entroq.Doc) (*Result, error)

// Result is what a DoModifyRun returns: the modifications the worker applies
// after work completes, plus optional work to run when the task is handled
// successfully. Build it with Modify, then chain OnSuccess for a post-success
// step.
type Result struct {
	mods         []entroq.ModifyArg
	onSuccess    func(context.Context) error
	onDependency func(context.Context, *entroq.DependencyError) error
}

// Modify begins a Result that applies args after work completes. The worker
// commits them at the stable (renewed) task version. Chain OnSuccess for an
// optimistic step that runs once the task is handled successfully.
func Modify(args ...entroq.ModifyArg) *Result {
	return &Result{mods: args}
}

// OnSuccess attaches fn to run after the task is handled successfully: the
// handler returned no error and any modifications it requested committed. It
// does NOT run if the handler returns an error or the commit fails -- the
// transaction is canceled and there is no success to build on. It is
// best-effort: its error is logged and never fails the task. To escalate a
// post-success failure, return a FatalError (FatalErrorf) and the worker stops
// after this task; RetryError/MoveError are meaningless here (nothing left to
// retry or move) and are treated as ordinary logged errors. fn receives only a
// context by design -- it is for optimistic post-success side effects (releasing
// a lock, deleting a self-owned marker), so retain anything it needs via a
// closure.
//
// OnSuccess is NOT a "finally" block: for cleanup that must run regardless of
// outcome, use a plain defer inside your handler function, which runs when the
// handler returns, before the worker commits.
func (r *Result) OnSuccess(fn func(context.Context) error) *Result {
	r.onSuccess = fn
	return r
}

// OnDependency attaches fn to run when the commit fails with a dependency error
// (task missing, already claimed, a depended-on doc gone, etc.). It runs after
// DoModify completes, so within a window that is less than a full claim period,
// but probably not much less than half of it for the task itself if the task was
// not implicated (the result would have succeeded but for some *other* task or
// document).
//
// The return value selects the task's disposition using the same sentinels as
// the work phase: a RetryError (optionally with After/OrMoveTo) re-queues with
// backoff and quarantines once attempts are exhausted; a MoveError quarantines
// immediately; a FatalError stops the worker; nil leaves the task to be
// reclaimed on lease expiry. Because the commit already failed and renewal has
// stopped, any such disposition is itself optimistic: it lands only if the task
// was not itself implicated in the failure (and so is still validly claimed).
//
// Returning any other (non-sentinel) error stops the worker: an unclassified
// failure leaves the loop in an unknown state, and crashing for the orchestrator
// to restart is safer than continuing from it. Classify the failures you
// understand as Retry/Move so only genuine surprises bring the worker down.
func (r *Result) OnDependency(fn func(context.Context, *entroq.DependencyError) error) *Result {
	r.onDependency = fn
	return r
}

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

// DoRun[T] is the WithDoWork function shape: the work phase. It is given the
// task, its typed value, and any claimed docs, but no client -- it runs under
// renewal, so it must not modify the claimed task (see Handler.Finish). Return a
// RetryError/MoveError to retry/move, or any other error to exit.
type DoRun[T any] func(context.Context, *entroq.Task, T, []*entroq.Doc) error

// FinishRun[T] is the WithFinish function shape: the commit phase. It runs after
// renewal has stopped and is handed a Modifier, so committing the (now stable)
// task is safe. It receives the same value and docs as DoRun.
type FinishRun[T any] func(context.Context, Modifier, *entroq.Task, T, []*entroq.Doc) error

// funcHandler[T] is a Handler[T] backed by plain functions.
type funcHandler[T any] struct {
	take   TakeRun[T]
	do     DoRun[T]
	finish FinishRun[T]
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
		return FatalErrorf("no work function specified")
	}
	return h.do(ctx, task, value, docs)
}

// Finish runs the specified "finish" function if it has been defined.
func (h *funcHandler[T]) Finish(ctx context.Context, mod Modifier, task *entroq.Task, value T, docs []*entroq.Doc) error {
	if h.finish == nil {
		return nil
	}
	return h.finish(ctx, mod, task, value, docs)
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
	take     TakeRun[T]
	doModify DoModifyRun[T]

	initialTask *entroq.Task
	result      *Result
}

func (h *doModifyHandler[T]) TakeDocs(ctx context.Context, task *entroq.Task, val T) ([]*entroq.DocClaim, error) {
	h.initialTask = task
	if h.take == nil {
		return nil, nil
	}
	return h.take(ctx, task, val)
}

func (h *doModifyHandler[T]) DoWork(ctx context.Context, task *entroq.Task, val T, docs []*entroq.Doc) error {
	if h.doModify == nil {
		return FatalErrorf("no work function specified")
	}
	result, err := h.doModify(ctx, task, val, docs)
	if err != nil {
		return err
	}
	h.result = result
	return nil
}

func (h *doModifyHandler[T]) Finish(ctx context.Context, mod Modifier, finalTask *entroq.Task, val T, finalDocs []*entroq.Doc) error {
	// initialTask is set unconditionally by TakeDocs, which always runs before
	// Finish, so it is non-nil here by construction.
	if h.result == nil {
		// Handler returned no Result: nothing to commit, nothing to run.
		return nil
	}

	if len(h.result.mods) > 0 {
		if finalTask == nil {
			return FatalErrorf("doModify finish: nil finalized task with modifications to apply")
		}

		modification := entroq.NewModification("", h.result.mods...)

		if h.initialTask.Version > finalTask.Version {
			return fmt.Errorf("task updated inside worker body, expected version <= %v, got %v", finalTask.Version, h.initialTask.Version)
		}

		// Fix up task modification versions to reflect the final renewed state.
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

		// Fix up doc modification versions to reflect the final renewed state.
		type nsID = [2]string
		docVers := make(map[nsID]int32, len(finalDocs))
		for _, d := range finalDocs {
			docVers[nsID{d.Namespace, d.ID}] = d.Version
		}
		for _, dc := range modification.DocChanges {
			if v, ok := docVers[nsID{dc.Namespace, dc.ID}]; ok {
				dc.Version = v
			}
		}
		for _, dd := range modification.DocDeletes {
			if v, ok := docVers[nsID{dd.Namespace, dd.ID}]; ok {
				dd.Version = v
			}
		}
		for _, dd := range modification.DocDepends {
			if v, ok := docVers[nsID{dd.Namespace, dd.ID}]; ok {
				dd.Version = v
			}
		}

		if _, err := mod.Modify(ctx, entroq.WithModification(modification)); err != nil {
			if depErr, ok := entroq.AsDependency(err); ok {
				log.Printf("Worker ack failed: %v", err)
				if fn := h.result.onDependency; fn != nil {
					// A returned Retry/Move/Fatal sentinel is honored by runOne
					// via handleSentinelErrors, exactly like a work-phase sentinel;
					// any other non-nil error stops the worker. A nil return falls
					// through to the default reclaim below.
					if ferr := fn(ctx, depErr); ferr != nil {
						return ferr
					}
				}
				return fmt.Errorf("worker doModify finish dependency: %w", err)
			}
			if entroq.IsCanceled(err) || entroq.IsTimeout(err) {
				log.Printf("Worker exiting cleanly instead of acking: %v", err)
				return fmt.Errorf("canceled doModify finish: %w", err)
			}
			return fmt.Errorf("worker doModify finish: %w", err)
		}
	}

	// The task was handled successfully (no error; any modifications committed).
	// OnSuccess is the optimistic post-success step: best-effort (its error is
	// logged), unless it returns a FatalError, which stops the worker.
	if h.result.onSuccess != nil {
		if err := h.result.onSuccess(ctx); err != nil {
			if _, ok := AsFatal(err); ok {
				return err
			}
			log.Printf("worker on-success: %v", err)
		}
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

	// Creates a new handler. Called once per task, in runOne, so per-task
	// handler state is isolated by construction.
	makeHandler MakeHandler[T]
	metrics     *workerMetrics
}

// workerOpts holds built-up worker options to be later checked against as a
// new worker is created.
type workerOpts[T any] struct {
	makeHandler MakeHandler[T]

	// These are all potential inputs to create the default handler.
	take     TakeRun[T]
	doModify DoModifyRun[T]
	do       DoRun[T]
	finish   FinishRun[T]

	errQMap ErrQMap
	mp      metric.MeterProvider
}

// New creates a new Worker[T] that claims tasks from its configured queues and
// presents pre-unmarshaled values of type T to the work handler.
//
// Options should be presented to, at a minimum, define the work to be done
// when a task is acquired. At least one of WithDoWork or WithDoModify should be
// specified, or WithMakeHandler if you have advanced needs (such as variable
// sharing between handler functions, which is not safe if specifying them as
// closures).
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
	if wOpts.mp != nil {
		metrics, err := newWorkerMetrics(wOpts.mp)
		if err != nil {
			log.Printf("worker metrics disabled: %v", err)
		} else {
			worker.metrics = metrics
		}
	}

	if worker.makeHandler != nil {
		return worker
	}

	// No makeHandler specified, build one from what we have.
	// DoModify handlers win. TakeDocs is always used.
	if wOpts.doModify != nil {
		worker.makeHandler = func() (Handler[T], error) {
			return &doModifyHandler[T]{
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

// WithMeterProvider enables worker slot state metrics on the supplied OTel
// provider. Concurrent calls to Run on this Worker are aggregated as slots.
func WithMeterProvider[T any](mp metric.MeterProvider) Option[T] {
	return func(wo *workerOpts[T]) {
		wo.mp = mp
	}
}

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

// WithDoWork sets the primary work function for a worker. It runs under
// background renewal and is given no client (see Handler.Finish). Overwrites any
// previous handler configuration.
func WithDoWork[T any](f DoRun[T]) Option[T] {
	return func(wo *workerOpts[T]) {
		wo.do = f
	}
}

// WithFinish sets the finalization function for a worker, called after DoWork
// completes successfully and renewal has stopped. The function receives the
// worker's client, the stable (finally-renewed) task, the original unmarshaled
// value, and any docs acquired by WithTakeDocs. Because it runs after renewal
// stops, modifying the task through the client is safe. Overwrites any previous
// handler configuration.
func WithFinish[T any](f FinishRun[T]) Option[T] {
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
// (moved to the error queue). Required docs claimed by another worker cause a
// backoff-and-retry
//
// When used with WithMakeHandler, the handler's TakeDocs method takes
// precedence and WithTakeDocs has no effect.
func WithTakeDocs[T any](f TakeRun[T]) Option[T] {
	return func(wo *workerOpts[T]) {
		wo.take = f
	}
}

// WithMakeHandler sets a "new" function to create a handler.
// Why use this instead of just setting a handler? If you are going to share
// any variables between docs, work, and finish functions, you want them to be
// fresh for each task, allowing concurrent Run calls and no surprises with
// internal handler state variables. Without specifying this, your handler will
// simply be used as is, all state shared not only between task loops, but also
// between Run calls. If you are calling Run multiple times to instantiate
// multipler concurrent workers, and you have any mutatable handler state, you
// MUST use this function for safety, or you MUST manage variables with
// mutexes.
//
// If you don't need this because you have no shared state, or you don't mind
// closure variable sharing, you can use more convenient approaches.
//
// Always available:
//
//   - WithTakeDocs - specifies how to identify documents for a particular task.
//
// Two approaches to defining work/finishing:
//
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
	if re, ok := AsRetry(sentinel); ok {
		delay := opts.baseRetryDelay
		if re.hasAfter {
			delay = re.after
		}
		q := errQ
		if re.moveTo != "" {
			q = re.moveTo
		}
		if _, err := w.eqc.Modify(ctx, task.RetryOrQuarantine(re.Error(), q, opts.maxAttempts, entroq.ArrivalTimeBy(delay))); err != nil {
			if _, ok := entroq.AsDependency(err); ok {
				// Optimistic: the task moved out from under us (already reclaimed
				// or handled elsewhere). Known state, not fatal -- log and continue.
				log.Printf("retry/quarantine skipped, task no longer ours: %v", err)
				return true, nil
			}
			return true, fmt.Errorf("retry or quarantine modify: %w", err)
		}
		return true, nil
	}
	if me, ok := AsMove(sentinel); ok {
		q := errQ
		if me.to != "" {
			q = me.to
		}
		if _, err := w.eqc.Modify(ctx, task.Quarantine(me.Error(), q)); err != nil {
			if _, ok := entroq.AsDependency(err); ok {
				// Optimistic: the task moved out from under us (already reclaimed
				// or handled elsewhere). Known state, not fatal -- log and continue.
				log.Printf("quarantine skipped, task no longer ours: %v", err)
				return true, nil
			}
			return true, fmt.Errorf("quarantine modify: %w", err)
		}
		return true, nil
	}
	if fe, ok := AsFatal(sentinel); ok {
		return true, fe
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
func (w *Worker[T]) runOne(ctx context.Context, opts *runOpt, slot *workerSlot) error {
	// Note: do NOT cancel rCtx from inside the work function. If rCtx is
	// canceled while a renewal Modify is in flight over gRPC, the client sees
	// context.Canceled but the server may have already committed the renewal.
	// The stopRenew/taskCh handoff in doWhileRenewing is the correct mechanism.
	rCtx, rCancel := context.WithCancel(ctx)
	defer rCancel()

	// Phase 1: Claim task and unmarshal its value.
	slot.set(workerIdle)
	task, err := w.eqc.Claim(rCtx, entroq.From(opts.qs...), entroq.ClaimFor(opts.lease))
	if err != nil {
		return fmt.Errorf("worker (%q) claim: %w", opts.qs, err)
	}
	slot.set(workerBusy)
	if opts.maxClaims > 0 && task.Claims > opts.maxClaims {
		errQ := w.ErrorQueueFor(task.Queue)
		if _, err := w.handleSentinelErrors(ctx,
			MoveErrorf("maximum claims exceeded (%d)", opts.maxClaims), task, errQ, opts,
		); err != nil {
			return fmt.Errorf("handle max claims: %w", err)
		}
		return nil
	}

	handler, err := w.makeHandler()
	if err != nil {
		return FatalErrorf("failed to make handler: %v", err)
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
				sentinelErr = MoveErrorf("required doc missing")
			} else {
				sentinelErr = RetryErrorf("doc contention")
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

	handleErr := doWhileRenewing(rCtx, w.eqc,
		func(ctx context.Context, stop finalizeRenew) error {
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
	if err := handler.Finish(ctx, w.eqc, finalTask, value, finalDocs); err != nil {
		// A post-commit hook (OnDependency) may return a Retry/Move/Fatal
		// sentinel; route it through the same machinery as a work-phase sentinel
		// before falling back to the default dependency reclaim.
		errQ := w.ErrorQueueFor(task.Queue)
		if isSentinel, serr := w.handleSentinelErrors(ctx, err, finalTask, errQ, opts); isSentinel {
			return serr
		}
		if de, ok := entroq.AsDependency(err); ok {
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
	maxClaims      int32
	lease          time.Duration
}

// Watching specifies the queues Run will watch.
func Watching(qs ...string) RunOption {
	return func(ro *runOpt) {
		ro.qs = qs
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

// WithMaxClaims sets the maximum number of times a task may be claimed before
// it is moved to the worker's error queue without constructing or invoking the
// handler. If 0 (the default), there is no maximum.
func WithMaxClaims(m int32) RunOption {
	return func(ro *runOpt) {
		ro.maxClaims = m
	}
}

// WithBaseRetryDelay sets the base delay for a retried task.
func WithBaseRetryDelay(d time.Duration) RunOption {
	return func(ro *runOpt) {
		ro.baseRetryDelay = d
	}
}

func isSentinelError(err error) bool {
	if _, ok := AsRetry(err); ok {
		return true
	}
	if _, ok := AsMove(err); ok {
		return true
	}
	_, ok := AsFatal(err)
	return ok
}

// Run claims tasks from the worker queues and processes them in a loop until its
// context is canceled or an unclassified error forces it to exit.
//
// Error disposition is a deliberate ladder, applied in order to whatever a
// handler or the commit returns:
//
//   - Sentinel (RetryError/MoveError/FatalError): acted on. Retry re-queues the
//     task with backoff, quarantining once max-attempts is reached; Move
//     quarantines it immediately; Fatal stops the worker. Retry and Move keep the
//     loop running, Fatal exits.
//   - DependencyError: reclaimed. The commit cleanly did not apply (a version
//     moved, a dependency was lost), a known state, so the task is left to be
//     re-claimed on lease expiry and the loop continues. See OnDependency to
//     inspect and redirect this case.
//   - Context cancellation or timeout: a clean stop; Run returns nil.
//   - Anything else: the worker exits. An unclassified error leaves the loop in an
//     unknown state, and crashing for an orchestrator to restart is safer, and
//     louder and thus more fixable, than continuing from state neither the author
//     nor the framework reasoned about. Classify the failures you understand as
//     Retry/Move so only genuine surprises bring the worker down.
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
	slot := w.metrics.add()
	defer slot.remove()
	for {
		if err := w.runOne(ctx, ro, slot); err != nil {
			if entroq.IsCanceled(err) || entroq.IsTimeout(err) {
				log.Printf("worker was asked to quit: %v", ctx.Err())
				return nil
			}
			return fmt.Errorf("worker (%q): %w", ro.qs, err)
		}
	}
}

// RetryError, returned from a worker handler, retries the claimed task: its
// attempt count is incremented and its arrival time is pushed into the future.
// Once the task exhausts its attempts (see WithMaxAttempts) it is moved to a
// quarantine queue instead. By default the delay is the worker's
// WithBaseRetryDelay and the quarantine queue comes from its ErrQMap; After and
// OrMoveTo override those per failure. Detect it with AsRetry.
type RetryError struct {
	msg      string
	after    time.Duration
	hasAfter bool
	moveTo   string
}

// Error implements the error interface.
func (e *RetryError) Error() string { return e.msg }

// RetryErrorf builds a RetryError with a printf-formatted message.
func RetryErrorf(format string, args ...any) *RetryError {
	return &RetryError{msg: fmt.Sprintf(format, args...)}
}

// After overrides the delay before this task is retried, in place of the
// worker's WithBaseRetryDelay. It is chainable.
func (e *RetryError) After(d time.Duration) *RetryError {
	e.after, e.hasAfter = d, true
	return e
}

// OrMoveTo overrides the queue this task is moved to once it exhausts its
// retries, in place of the worker's ErrQMap. It is chainable.
func (e *RetryError) OrMoveTo(queue string) *RetryError {
	e.moveTo = queue
	return e
}

// AsRetry reports whether err is, or wraps, a *RetryError.
func AsRetry(err error) (*RetryError, bool) {
	var e *RetryError
	return e, errors.As(err, &e)
}

// MoveError, returned from a worker handler, moves the claimed task to another
// queue immediately: its attempt count is incremented, the error is recorded,
// and it is requeued for inspection. Use it for a task that will not do better
// with a retry. By default the destination is the worker's ErrQMap; To
// overrides it. Detect it with AsMove.
type MoveError struct {
	msg string
	to  string
}

// Error implements the error interface.
func (e *MoveError) Error() string { return e.msg }

// MoveErrorf builds a MoveError with a printf-formatted message.
func MoveErrorf(format string, args ...any) *MoveError {
	return &MoveError{msg: fmt.Sprintf(format, args...)}
}

// To overrides the queue this task is moved to, in place of the worker's
// ErrQMap. It is chainable.
func (e *MoveError) To(queue string) *MoveError {
	e.to = queue
	return e
}

// AsMove reports whether err is, or wraps, a *MoveError.
func AsMove(err error) (*MoveError, bool) {
	var e *MoveError
	return e, errors.As(err, &e)
}

// FatalError, returned from a worker handler, stops the worker immediately. Use
// it when the worker cannot or should not keep processing tasks. Detect it with
// AsFatal.
type FatalError struct {
	msg string
}

// Error implements the error interface.
func (e *FatalError) Error() string { return e.msg }

// FatalErrorf builds a FatalError with a printf-formatted message.
func FatalErrorf(format string, args ...any) *FatalError {
	return &FatalError{msg: fmt.Sprintf(format, args...)}
}

// AsFatal reports whether err is, or wraps, a *FatalError.
func AsFatal(err error) (*FatalError, bool) {
	var e *FatalError
	return e, errors.As(err, &e)
}

// Renewal Machinery

// finalizeRenew is a function that can be called to stop renewal from a
// worker routine. It returns a RenewResponse with tasks and/or docs with
// stable versions (no longer renewing).
type finalizeRenew func() *entroq.RenewResponse

// workFn handles tasks and docs while renewal runs in the background.
type workFn func(ctx context.Context, stop finalizeRenew) error

// doWhileRenewing runs the given work function while keeping the provided tasks and docs claimed in the background.
func doWhileRenewing(ctx context.Context, c *entroq.EntroQ, work workFn, opts ...entroq.RenewOption) error {
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
		if err := work(fctx, finalize); err != nil {
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
