// Package worker provides a high-level looping protocol for processing tasks.
//
// This package is the reference implementation of EntroQ's worker semantics.
// The Python and JS clients are ports of it, and pkg/workgateway hosts foreign
// workers on it directly; a client that disagrees with this package is wrong by
// definition. See AGENTS.md before changing behavior here, because a change
// here is a change to every client's contract.
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
//		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, value json.RawMessage, _ []*entroq.DocSet) (*worker.Result, error) {
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
	"strings"
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
	// doc sets the worker needs to claim ownership of before doing work. Return
	// nil to skip doc acquisition. Each claim takes a whole set, which may have
	// no docs yet. A set someone else holds causes a retry.
	//
	// Note: renewal of the task and its sets begins once TakeDocs returns and
	// the sets are claimed, so a slow TakeDocs spends the task's first lease. In
	// natural use, where TakeDocs just returns a list of DocClaim specs without
	// doing I/O, this is negligible.
	TakeDocs(context.Context, *entroq.Task, T) ([]*entroq.DocClaim, error)

	// DoWork is called by Worker.Run for each claimed task. The task and its
	// doc sets, empty ones included, are renewed together in the background
	// while this function runs. value holds the result of
	// unmarshaling task.Value into T. The sets are those TakeDocs claimed, in
	// the order they were claimed (by namespace, then key), each with its docs;
	// the slice is non-nil but empty when none were claimed, and a set may have
	// no docs.
	//
	// On nil return, renewal is stopped and Finish (if set) is called with
	// the stable task version.
	//
	// On RetryError or MoveError, the task is retried or moved and Finish
	// is skipped. In both cases, the task's availability is set in the future
	// and its attempt count is incremented (these errors, while convenient for
	// managing task movement, are still errors), and its doc sets are released.
	//
	// On any other error, Finish is skipped and the worker exits. Backoff and
	// restart are the responsibility of the process orchestrator (e.g.
	// Kubernetes, systemd). To retry or quarantine the task instead, return a
	// RetryError or MoveError (see RetryErrorf and MoveErrorf).
	DoWork(context.Context, *entroq.Task, T, []*entroq.DocSet) error

	// Finish is called after DoWork returns nil and renewal has stopped. It
	// receives a Modifier (the worker's client, narrowed to modification), the
	// stable (final renewed) task version, the same value passed to DoWork, and
	// the same doc sets, at their final versions. Use it to apply task modifications -- deletion, requeueing,
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
	Finish(context.Context, Modifier, *entroq.Task, T, []*entroq.DocSet) error
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
// The sets parameter carries any doc sets claimed by WithTakeDocs, and can
// be empty.
//
// Return the Result (built with Modify) and a nil error on success; a nil Result
// is a valid no-op. To retry the task return a RetryError (RetryErrorf); to move
// it return a MoveError (MoveErrorf). Any other non-nil error causes the worker
// to exit -- backoff and restart are the responsibility of the process
// orchestrator.
type DoModifyRun[T any] func(context.Context, *entroq.Task, T, []*entroq.DocSet) (*Result, error)

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
// commits them at the stable (renewed) versions of the task and its doc sets.
// Chain OnSuccess for an optimistic step that runs once the task is handled
// successfully.
//
// Doc sets follow their task: when the commit writes the task (changes,
// deletes, or makes it arrive, rather than only depending on it), the worker
// releases each set the commit did not write once OnSuccess has run, so other
// workers can claim it at once. A set the commit wrote keeps the arrival the
// commit gave it. The release is best-effort: one that fails is logged, and
// its sets wait out their leases.
//
// The usual result deletes the task, or moves it on. A result that leaves the
// task in its queue ready now, such as one that only makes it arrive now
// (entroq.Arriving with entroq.ReadyNow), hands it straight back to the next
// claim, very likely this worker's: like a loop that never changes its exit
// condition, it spins. Leave the task where it cannot be claimed again at
// once, or give it a later arrival. A commit that only makes the task arrive
// is counted as released rather than done.
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
// task, its typed value, and any claimed doc sets, but no client -- it runs under
// renewal, so it must not modify the claimed task (see Handler.Finish). Return a
// RetryError/MoveError to retry/move, or any other error to exit.
type DoRun[T any] func(context.Context, *entroq.Task, T, []*entroq.DocSet) error

// FinishRun[T] is the WithFinish function shape: the commit phase. It runs after
// renewal has stopped and is handed a Modifier, so committing the (now stable)
// task is safe. It receives the same value and doc sets as DoRun.
type FinishRun[T any] func(context.Context, Modifier, *entroq.Task, T, []*entroq.DocSet) error

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
func (h *funcHandler[T]) DoWork(ctx context.Context, task *entroq.Task, value T, sets []*entroq.DocSet) error {
	if h.do == nil {
		return FatalErrorf("no work function specified")
	}
	return h.do(ctx, task, value, sets)
}

// Finish runs the specified "finish" function if it has been defined.
func (h *funcHandler[T]) Finish(ctx context.Context, mod Modifier, task *entroq.Task, value T, sets []*entroq.DocSet) error {
	if h.finish == nil {
		return nil
	}
	return h.finish(ctx, mod, task, value, sets)
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
	released    bool // the commit only made the task arrive
}

func (h *doModifyHandler[T]) TakeDocs(ctx context.Context, task *entroq.Task, val T) ([]*entroq.DocClaim, error) {
	h.initialTask = task
	if h.take == nil {
		return nil, nil
	}
	return h.take(ctx, task, val)
}

func (h *doModifyHandler[T]) DoWork(ctx context.Context, task *entroq.Task, val T, sets []*entroq.DocSet) error {
	if h.doModify == nil {
		return FatalErrorf("no work function specified")
	}
	result, err := h.doModify(ctx, task, val, sets)
	if err != nil {
		return err
	}
	h.result = result
	return nil
}

func (h *doModifyHandler[T]) Finish(ctx context.Context, mod Modifier, finalTask *entroq.Task, val T, finalSets []*entroq.DocSet) error {
	// initialTask is set unconditionally by TakeDocs, which always runs before
	// Finish, so it is non-nil here by construction.
	if h.result == nil {
		// Handler returned no Result: nothing to commit, nothing to run.
		return nil
	}

	var release []*entroq.DocSet
	if len(h.result.mods) > 0 {
		if finalTask == nil {
			return FatalErrorf("doModify finish: nil finalized task with modifications to apply")
		}
		if h.initialTask.Version > finalTask.Version {
			return fmt.Errorf("task updated inside worker body, expected version <= %v, got %v", finalTask.Version, h.initialTask.Version)
		}

		modification := entroq.NewModification("", h.result.mods...)
		fixVersions(modification, finalTask, finalSets)
		if _, err := mod.Modify(ctx, entroq.WithModification(modification)); err != nil {
			return h.commitFailed(ctx, err)
		}
		// The sets follow the task: once the commit lets go of it, the sets
		// the commit did not write are released too, after OnSuccess, which
		// may still use them.
		if writesTask(modification, finalTask.ID) {
			release = untouched(modification, finalSets)
		}
		h.released = onlyArrives(modification, finalTask.ID)
	}

	// The task was handled successfully (no error; any modifications committed).
	// OnSuccess is the optimistic post-success step: best-effort (its error is
	// logged), unless it returns a FatalError, which stops the worker.
	var fatal error
	if h.result.onSuccess != nil {
		if err := h.result.onSuccess(ctx); err != nil {
			if _, ok := AsFatal(err); ok {
				fatal = err
			} else {
				log.Printf("worker on-success: %v", err)
			}
		}
	}
	releaseSets(ctx, mod, release)
	return fatal
}

// commitFailed handles a commit of the handler's result that did not apply.
func (h *doModifyHandler[T]) commitFailed(ctx context.Context, err error) error {
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

// fixVersions moves m's operations on the task and its doc sets to the
// versions renewal left them at. Every doc in a set is at the set's version.
func fixVersions(m *entroq.Modification, task *entroq.Task, sets []*entroq.DocSet) {
	for _, t := range m.Changes {
		if t.ID == task.ID {
			t.Version = task.Version
		}
	}
	for _, t := range m.Depends {
		if t.ID == task.ID {
			t.Version = task.Version
		}
	}
	for _, t := range m.Deletes {
		if t.ID == task.ID {
			t.Version = task.Version
		}
	}
	for _, a := range m.Arrives {
		if a.ID == task.ID {
			a.Version = task.Version
		}
	}

	setVers := make(map[setKey]int32, len(sets))
	for _, g := range sets {
		setVers[setKey{g.Namespace, g.Key}] = g.Version
	}
	docVers := make(map[docKey]int32)
	for _, d := range entroq.DocsIn(sets) {
		docVers[docKey{d.Namespace, d.ID}] = d.Version
	}
	for _, dc := range m.DocChanges {
		if v, ok := docVers[docKey{dc.Namespace, dc.ID}]; ok {
			dc.Version = v
		}
	}
	for _, dd := range m.DocDeletes {
		if v, ok := docVers[docKey{dd.Namespace, dd.ID}]; ok {
			dd.Version = v
		}
	}
	for _, dd := range m.DocDepends {
		if v, ok := docVers[docKey{dd.Namespace, dd.ID}]; ok {
			dd.Version = v
		}
	}
	for _, a := range m.DocArrives {
		if v, ok := setVers[setKey{a.Namespace, a.Key}]; ok {
			a.Version = v
		}
	}
}

type (
	setKey struct{ ns, key string }
	docKey struct{ ns, id string }
)

// writesTask reports whether m changes, deletes, or makes arrive the task
// with the given ID, rather than only depending on it.
func writesTask(m *entroq.Modification, id string) bool {
	return rewritesTask(m, id) || arrives(m, id)
}

// rewritesTask reports whether m changes or deletes the task with the given
// ID.
func rewritesTask(m *entroq.Modification, id string) bool {
	for _, t := range m.Changes {
		if t.ID == id {
			return true
		}
	}
	for _, t := range m.Deletes {
		if t.ID == id {
			return true
		}
	}
	return false
}

// arrives reports whether m makes the task with the given ID arrive.
func arrives(m *entroq.Modification, id string) bool {
	for _, a := range m.Arrives {
		if a.ID == id {
			return true
		}
	}
	return false
}

// onlyArrives reports whether m makes the task with the given ID arrive
// without changing or deleting it: a release, or a deferral.
func onlyArrives(m *entroq.Modification, id string) bool {
	return arrives(m, id) && !rewritesTask(m, id)
}

// untouched returns the sets m does not write: none of their docs is
// inserted, changed, or deleted, and the set does not arrive. Depending on a
// doc does not write its set.
func untouched(m *entroq.Modification, sets []*entroq.DocSet) []*entroq.DocSet {
	memberOf := make(map[docKey]setKey)
	for _, g := range sets {
		for _, d := range g.Docs {
			memberOf[docKey{d.Namespace, d.ID}] = setKey{g.Namespace, g.Key}
		}
	}
	written := make(map[setKey]bool)
	for _, ins := range m.DocInserts {
		written[setKey{ins.Namespace, ins.Key}] = true
	}
	for _, dc := range m.DocChanges {
		written[setKey{dc.Namespace, dc.Key}] = true
	}
	for _, dd := range m.DocDeletes {
		if k, ok := memberOf[docKey{dd.Namespace, dd.ID}]; ok {
			written[k] = true
		}
	}
	for _, a := range m.DocArrives {
		written[setKey{a.Namespace, a.Key}] = true
	}
	var out []*entroq.DocSet
	for _, g := range sets {
		if !written[setKey{g.Namespace, g.Key}] {
			out = append(out, g)
		}
	}
	return out
}

// releaseSets makes sets ready again now, after a commit that let go of the
// task holding them. It is best-effort: a set it cannot release waits out its
// lease, as it would have without it.
func releaseSets(ctx context.Context, mod Modifier, sets []*entroq.DocSet) {
	if len(sets) == 0 {
		return
	}
	if _, err := mod.Modify(ctx, entroq.Arriving(entroq.ReadyNow().Docs(sets...))); err != nil {
		log.Printf("worker: release doc sets after commit: %v", err)
	}
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

	// Shutdown state, like http.Server's tracked connections: each Run joins
	// runs under mu, so Shutdown can reach it, and none joins once closed is
	// set, so wg.Add never races wg.Wait.
	mu     sync.Mutex
	closed bool
	runs   map[*activeRun]struct{}
	wg     sync.WaitGroup
}

// activeRun holds what Shutdown needs to stop one Run.
type activeRun struct {
	cancel      context.CancelFunc // ends the Run, canceling its handler
	cancelClaim context.CancelFunc // ends its claim; nil when not claiming
}

// ErrShutdown is returned by Run on a worker that Shutdown has been called on.
var ErrShutdown = errors.New("worker: shut down")

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
		runs:        make(map[*activeRun]struct{}),
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

// ErrQTemplate returns the error queue mapping a template names: "{inbox}" in
// it stands for the inbox, so "{inbox}/err" is DefaultErrQMap, and a template
// without it is one error queue for every inbox. An empty template is
// DefaultErrQMap. Command-line workers take their error queue flag this way.
func ErrQTemplate(template string) ErrQMap {
	if template == "" {
		return DefaultErrQMap
	}
	return func(inbox string) string {
		return strings.ReplaceAll(template, "{inbox}", inbox)
	}
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
// function is called with the claimed task to declare which doc sets are
// needed. A set claimed by another worker causes a backoff-and-retry. A
// set may have no docs yet; the handler decides what an empty set means,
// and can return a MoveError if it should have had some.
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

// handleSentinelErrors commits the disposition a sentinel error asks for. A
// retry or move lets go of the task, so the doc sets it held, in sets, are
// released after it commits.
func (w *Worker[T]) handleSentinelErrors(ctx context.Context, sentinel error, task *entroq.Task, sets []*entroq.DocSet, errQ string, opts *runOpt) (isSentinel bool, err error) {
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
		releaseSets(ctx, w.eqc, sets)
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
		releaseSets(ctx, w.eqc, sets)
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
// It returns the claimed sets in claim order, sets with no docs included.
//
// Returns a *entroq.DependencyError while another claimant holds a set; the
// caller retries the task with backoff.
func acquireDocs[T any](ctx context.Context, eqc *entroq.EntroQ, task *entroq.Task, value T, lease time.Duration, take TakeRun[T]) ([]*entroq.DocSet, error) {
	sets := []*entroq.DocSet{}
	if take == nil {
		return sets, nil
	}
	req, err := take(ctx, task, value)
	if err != nil {
		return nil, fmt.Errorf("take docs: %w", err)
	}
	if req == nil {
		return sets, nil
	}

	// Sort to avoid livelock from dining philosophers.
	sort.Slice(req, func(i, j int) bool {
		if req[i].Namespace != req[j].Namespace {
			return req[i].Namespace < req[j].Namespace
		}
		return req[i].Key < req[j].Key
	})

	for _, cq := range req {
		cq.Duration = lease
		set, err := eqc.ClaimDocs(ctx, cq)
		if err != nil {
			return nil, err // caller inspects DependencyError
		}
		sets = append(sets, set)
	}
	return sets, nil
}

// runOne claims one task, unmarshals its value into T, runs the work function
// with renewal, and applies any resulting modification.
func (w *Worker[T]) runOne(ctx context.Context, run *activeRun, opts *runOpt, slot *workerSlot) error {
	// Note: do NOT cancel rCtx from inside the work function. If rCtx is
	// canceled while a renewal Modify is in flight over gRPC, the client sees
	// context.Canceled but the server may have already committed the renewal.
	// The stopRenew/taskCh handoff in doWhileRenewing is the correct mechanism.
	rCtx, rCancel := context.WithCancel(ctx)
	defer rCancel()

	// Phase 1: Claim task and unmarshal its value.
	slot.set(workerIdle)
	claimCtx, endClaim, err := w.startClaim(rCtx, run)
	if err != nil {
		return err
	}
	task, err := w.eqc.Claim(claimCtx, entroq.From(opts.qs...), entroq.ClaimFor(opts.lease))
	claimed := time.Now()
	endClaim()
	if err != nil {
		return fmt.Errorf("worker (%q) claim: %w", opts.qs, err)
	}
	slot.set(workerBusy)

	// Count every claimed task once, by how it ended. A task is deleted when it
	// completes, so the queue keeps no record of who handled what; this counter
	// is where that history lives. Default to "failed" so an exit path added
	// later is counted pessimistically rather than silently dropped.
	outcome := outcomeFailed
	defer func() { w.metrics.recordTask(ctx, task.Queue, w.eqc.ID(), outcome) }()

	if opts.maxClaims > 0 && task.Claims > opts.maxClaims {
		errQ := w.ErrorQueueFor(task.Queue)
		if _, err := w.handleSentinelErrors(ctx,
			MoveErrorf("maximum claims exceeded (%d)", opts.maxClaims), task, nil, errQ, opts,
		); err != nil {
			return fmt.Errorf("handle max claims: %w", err)
		}
		outcome = outcomeMoved
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
	// doc sets are acquired.
	sets, err := acquireDocs(rCtx, w.eqc, task, value, opts.lease, handler.TakeDocs)
	if err != nil {
		// A claim fails only while someone else holds the set, which is
		// transient: retry with backoff. Sets claimed before it keep their
		// leases.
		if _, ok := entroq.AsDependency(err); ok {
			outcome = outcomeRetried
			errQ := w.ErrorQueueFor(task.Queue)
			if _, herr := w.handleSentinelErrors(ctx, RetryErrorf("doc contention"), task, nil, errQ, opts); herr != nil {
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
		finalSets   []*entroq.DocSet
	)

	// Renew first half a lease after the claim, at once if claiming the sets
	// took longer: the task's lease has run since then.
	first := max(0, opts.lease/2-time.Since(claimed))
	handleErr := doWhileRenewing(rCtx, w.eqc, opts.lease, first, held{task: task, sets: sets},
		func(ctx context.Context, stop finalizeRenew) error {
			defer func() {
				final := stop()
				finalTask, finalSets = final.task, final.sets
			}()
			if err := handler.DoWork(ctx, task, value, sets); err != nil {
				if !isSentinelError(err) {
					return fmt.Errorf("task do: %w", err)
				}
				sentinelErr = err
			}
			return nil
		},
	)

	if sentinelErr != nil {
		outcome = sentinelOutcome(sentinelErr)
		errQ := w.ErrorQueueFor(task.Queue)
		if _, err := w.handleSentinelErrors(ctx, sentinelErr, finalTask, finalSets, errQ, opts); err != nil {
			return fmt.Errorf("handle sentinel error: %w", err)
		}
		return nil
	}

	if handleErr != nil {
		return fmt.Errorf("worker (%q): %w", opts.qs, handleErr)
	}

	// Phase 4: Finish with stable versions — renewal has stopped.
	if err := handler.Finish(ctx, w.eqc, finalTask, value, finalSets); err != nil {
		// A post-commit hook (OnDependency) may return a Retry/Move/Fatal
		// sentinel; route it through the same machinery as a work-phase sentinel
		// before falling back to the default dependency reclaim.
		errQ := w.ErrorQueueFor(task.Queue)
		if isSentinel, serr := w.handleSentinelErrors(ctx, err, finalTask, finalSets, errQ, opts); isSentinel {
			return serr
		}
		if de, ok := entroq.AsDependency(err); ok {
			log.Printf("Worker finish failed (%q), throwing away: %v", opts.qs, de)
			outcome = outcomeRetried
			return nil
		}
		if entroq.IsTimeout(err) || entroq.IsCanceled(err) {
			log.Printf("Worker exiting cleanly: %v", err)
			return fmt.Errorf("canceled in finish: %w", err)
		}
		return fmt.Errorf("worker finish (%q): %w", opts.qs, err)
	}
	outcome = outcomeDone
	if h, ok := handler.(*doModifyHandler[T]); ok && h.released {
		outcome = outcomeReleased
	}
	return nil
}

// sentinelOutcome maps a handler sentinel onto the outcome it produces.
func sentinelOutcome(err error) string {
	if _, ok := AsMove(err); ok {
		return outcomeMoved
	}
	if _, ok := AsRetry(err); ok {
		return outcomeRetried
	}
	return outcomeFailed
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
//   - Context cancellation or timeout: a clean stop; Run returns nil. Shutdown
//     stops Run the same way, after its task is done.
//   - Anything else: the worker exits. An unclassified error leaves the loop in an
//     unknown state, and crashing for an orchestrator to restart is safer, and
//     louder and thus more fixable, than continuing from state neither the author
//     nor the framework reasoned about. Classify the failures you understand as
//     Retry/Move so only genuine surprises bring the worker down.
//
// A claimant is a consumer, not a process. Run claims as the worker's client
// ID, so concurrent Runs of one worker, or of workers sharing a client, are
// one claimant to EntroQ: it cannot tell them apart. Two of them can then
// hold the same doc set at once, and each invalidates the versions the other
// holds whenever it renews, commits, or releases the set. Give Runs that
// claim doc sets their own clients (see entroq.WithClaimantID), or keep the
// sets they claim apart.
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
	if err := w.checkProtocol(ctx); err != nil {
		return fmt.Errorf("worker (%q): %w", ro.qs, err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	run := &activeRun{cancel: cancel}
	if err := w.join(run); err != nil {
		return err
	}
	defer w.leave(run)

	slot := w.metrics.add()
	defer slot.remove()
	for {
		if err := w.runOne(ctx, run, ro, slot); err != nil {
			if errors.Is(err, ErrShutdown) {
				return nil
			}
			if entroq.IsCanceled(err) || entroq.IsTimeout(err) {
				log.Printf("worker (%q) was asked to quit: %v", ro.qs, err)
				return nil
			}
			return fmt.Errorf("worker (%q): %w", ro.qs, err)
		}
	}
}

// minServerProtocol is the oldest wire protocol a worker can run against: it
// renews and releases with lease-only changes, which protocol 2 added.
const minServerProtocol = 2

// checkProtocol refuses a server too old for the worker. Servers upgrade
// before their clients, so this is a deployment mistake, and reported as one
// before anything is claimed.
func (w *Worker[T]) checkProtocol(ctx context.Context) error {
	p, err := w.eqc.ServerProtocol(ctx)
	if err != nil {
		return fmt.Errorf("check server protocol: %w", err)
	}
	if p < minServerProtocol {
		return entroq.Unsupportedf("the EntroQ server speaks protocol %d, and this worker needs protocol %d or later: upgrade the server first", p, minServerProtocol)
	}
	return nil
}

// join registers run for Shutdown, or refuses once Shutdown has been called.
func (w *Worker[T]) join(run *activeRun) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrShutdown
	}
	w.runs[run] = struct{}{}
	w.wg.Add(1)
	return nil
}

// leave unregisters a finished run.
func (w *Worker[T]) leave(run *activeRun) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.runs, run)
	w.wg.Done()
}

// startClaim returns a context for run's next claim that Shutdown can cancel,
// and a function to call when the claim returns. Checking closed here, under
// mu, means a claim either starts before Shutdown and is canceled by it, or
// does not start at all. Shutdown cancels only the claim: a task it has
// already returned is worked, as part of draining.
func (w *Worker[T]) startClaim(ctx context.Context, run *activeRun) (context.Context, func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, nil, ErrShutdown
	}
	ctx, cancel := context.WithCancel(ctx)
	run.cancelClaim = cancel
	return ctx, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		run.cancelClaim = nil
		cancel()
	}, nil
}

// Shutdown stops the worker gracefully, like http.Server.Shutdown: every Run
// stops claiming at once, finishes the task it holds (renewing and committing
// it as usual), and returns nil. Shutdown waits for all of them. If ctx ends
// first, it cancels the handlers still running and returns ctx.Err() without
// waiting further; their tasks are reclaimed when their leases expire.
//
// A claim canceled by Shutdown that the server had already granted cannot be
// returned, so that task waits out its lease too. After Shutdown, Run returns
// ErrShutdown.
func (w *Worker[T]) Shutdown(ctx context.Context) error {
	w.closeRuns(func(run *activeRun) {
		if run.cancelClaim != nil {
			run.cancelClaim()
		}
	})

	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		w.closeRuns(func(run *activeRun) { run.cancel() })
		return ctx.Err()
	}
}

// closeRuns marks the worker closed, so no Run joins or claims again, and
// calls stop on each active Run while no Run can leave.
func (w *Worker[T]) closeRuns(stop func(*activeRun)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	for run := range w.runs {
		stop(run)
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

// held is what a worker holds while it works: its task and the doc sets it
// claimed, at their latest versions.
type held struct {
	task *entroq.Task
	sets []*entroq.DocSet
}

// renewed returns h as a renewal left it, from its response: the task at its
// new version, and each set, and its docs, at the set's new lock.
func (h held) renewed(resp *entroq.ModifyResponse) (held, error) {
	if len(resp.ChangedTasks) != 1 || resp.ChangedTasks[0].ID != h.task.ID {
		return held{}, fmt.Errorf("renewal of task %s returned tasks %v", h.task.ID, resp.ChangedTasks)
	}
	locks := make(map[setKey]*entroq.DocSet, len(resp.ChangedSets))
	for _, l := range resp.ChangedSets {
		locks[setKey{l.Namespace, l.Key}] = l
	}
	out := held{task: resp.ChangedTasks[0], sets: make([]*entroq.DocSet, 0, len(h.sets))}
	for _, g := range h.sets {
		l, ok := locks[setKey{g.Namespace, g.Key}]
		if !ok {
			return held{}, fmt.Errorf("renewal did not return doc set %q in %q", g.Key, g.Namespace)
		}
		out.sets = append(out.sets, relocked(g, l))
	}
	return out, nil
}

// relocked returns g, and each of its docs, at lock l.
func relocked(g, l *entroq.DocSet) *entroq.DocSet {
	ng := *g
	ng.Version, ng.Claimant, ng.At, ng.NumDocs = l.Version, l.Claimant, l.At, l.NumDocs
	ng.Docs = make([]*entroq.Doc, 0, len(g.Docs))
	for _, d := range g.Docs {
		nd := *d
		nd.Version, nd.Claimant, nd.At = l.Version, l.Claimant, l.At
		ng.Docs = append(ng.Docs, &nd)
	}
	return &ng
}

// finalizeRenew stops renewal from a worker routine and returns what it held,
// at the stable versions it left them at.
type finalizeRenew func() held

// workFn handles tasks and docs while renewal runs in the background.
type workFn func(ctx context.Context, stop finalizeRenew) error

// doWhileRenewing runs work while keeping h claimed in the background: after
// first, and then every half lease, one UpdateArrival renews the task and
// every set, sets with no docs included. A renewal that finds the claim lost, a server that cannot
// renew, or a renewal that does not return what it renewed cancels work with
// that error as its cause.
func doWhileRenewing(ctx context.Context, c *entroq.EntroQ, lease, first time.Duration, h held, work workFn) error {
	if h.task == nil {
		return fmt.Errorf("do while renewing: nothing to renew")
	}
	type outVal struct {
		held held
		err  error
	}
	taskCh := make(chan outVal, 1)

	g, ctx := errgroup.WithContext(ctx)

	fctx, fcancel := context.WithCancelCause(ctx)
	defer fcancel(nil)

	stopRenew := make(chan struct{})
	g.Go(func() error {
		cur := h
		next := first
		var out chan<- outVal
		var stopErr error
		stop := func(err error) {
			fcancel(err)
			stopErr = err
			out = taskCh
		}
		doneCh := ctx.Done()
		for {
			select {
			case <-stopRenew:
				out = taskCh
				stopRenew = nil
			case <-doneCh:
				out = taskCh
				doneCh = nil
			case <-time.After(next):
				next = lease / 2
				if stopErr != nil {
					break
				}
				resp, err := c.UpdateArrival(ctx, entroq.ReadyIn(lease).Tasks(cur.task).Docs(cur.sets...))
				_, lost := entroq.AsDependency(err)
				switch {
				case err == nil:
					next, err := cur.renewed(resp)
					if err != nil {
						stop(err)
						break
					}
					cur = next
				case entroq.IsCanceled(err):
					out = taskCh
				case lost || entroq.IsUnsupported(err):
					// The claim is gone, or the server cannot renew it:
					// nothing holds the work any longer.
					stop(err)
				default:
					log.Printf("Transient renewal error: %v", err)
				}
			case out <- outVal{cur, stopErr}:
				return nil
			}
		}
	})

	// finalize is safe to call any number of times from any goroutine.
	// sync.Once ensures stopRenew is closed exactly once and taskCh is read
	// exactly once; subsequent calls return the already-captured result.
	var (
		finalizeOnce sync.Once
		final        held
	)
	finalize := func() held {
		finalizeOnce.Do(func() {
			close(stopRenew)
			out := <-taskCh
			if out.err != nil {
				fcancel(out.err)
			}
			final = out.held
		})
		return final
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
