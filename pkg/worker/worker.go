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

// DefaultWorkTimeout bounds a handler body unless WithWorkTimeout says
// otherwise. A body that runs past it is asked to stop, and its task is
// retried with its attempt counted.
//
// It is deliberately finite. An unbounded body that hangs holds its task under
// renewal for as long as the worker lives, and no other worker can ever have
// it, so the default that costs nothing when work is quick is the one that
// catches that. Five minutes is long enough that ordinary work never notices
// and short enough that a wedged worker is found the same day.
//
// Work that genuinely takes longer is either worth breaking into pieces, or
// knows exactly why it is long -- a model occupying a GPU for a quarter of an
// hour -- and says so with WithWorkTimeout, passing zero for no bound.
const DefaultWorkTimeout = 5 * time.Minute

// Handler[T] is an interface that can be implemented to define work to be done.
// The value T is the pre-unmarshaled task value. Use T = json.RawMessage to
// receive raw bytes without any type-level unmarshaling.
//
// The methods correspond to the phases of task processing, in this order:
//   - TakeDocs: pre-work doc acquisition (optional; return nil to skip)
//   - CueWork: tell whoever does the work that it is coming (optional)
//   - DoWork: primary work, runs with background renewal
//   - Finish: commit phase, runs after renewal stops with stable task version
//
// The first two run on the lease the claim GRANTED, which nothing is extending
// yet; renewal starts with DoWork and stops before Finish. That one fact
// explains every signature here: what a phase is handed, and what it is safe
// for that phase to do, follow from whether anything is holding the claim open
// while it runs.
type Handler[T any] interface {
	// TakeDocs is called after a task is claimed and before DoWork. It declares which
	// doc sets the worker needs to claim ownership of before doing work. Return
	// nil to skip doc acquisition. Each claim takes a whole set, which may have
	// no docs yet. A set someone else holds causes a retry.
	//
	// Note: renewal of the task and its sets begins once TakeDocs returns and
	// the sets are claimed, so a slow TakeDocs spends the task's first lease. In
	// natural use, where TakeDocs just returns Take of some ClaimKey sets without
	// doing I/O, this is negligible.
	TakeDocs(context.Context, entroq.Reader, *Work[T]) (*TakeResult, error)

	// CueWork is called once the task's doc sets are held and before renewal
	// begins: the cue that work is about to start, given to whoever will do it
	// so that it can be ready. Its Work carries the task, its value and the
	// held sets. Return nil to skip it.
	//
	// NOTHING IS RENEWING THE CLAIM WHILE IT RUNS, which is the reason this
	// phase exists apart from DoWork. A cue that never completes blocks this
	// worker, and the task's lease simply runs out, so another worker takes
	// it. The same wait inside DoWork would pin the task for as long as the
	// worker lived, because renewal would keep extending a claim nobody was
	// acting on. Reaching into another process to hand work over belongs
	// here.
	//
	// A RetryError or MoveError acts on the task exactly as it does from
	// TakeDocs; any other error stops the worker.
	CueWork(context.Context, entroq.Reader, *Work[T]) error

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
	DoWork(context.Context, entroq.Reader, *Work[T]) error

	// Finish is called after DoWork returns nil and renewal has stopped. Its
	// Work carries the stable (final renewed) task and the doc sets at their
	// final versions. Use it to apply task modifications -- deletion,
	// requeueing, doc changes. Finish is skipped when DoWork returns a non-nil
	// error of any kind.
	Finish(context.Context, entroq.Client, *Work[T]) error
}

// Work[T] is what a handler phase is told about the task in hand: data, and
// only data. The behavior a phase needs -- reading state, committing -- arrives
// as the entroq.Client beside it, which is what keeps this from growing
// methods and keeps the two separable for a test.
//
// It is a struct rather than an interface so that fields can be added without
// breaking anything, and exported so a test can build one and call a handler
// directly instead of standing up a worker.
//
// The worker takes its own final task and sets from the renewal handoff rather
// than from here, so a handler that overwrites a field confuses only itself.
type Work[T any] struct {
	// Task is the claimed task, at the version the phase begins with. Under
	// renewal that version moves, which is why a commit's versions are fixed
	// by the worker rather than by the handler.
	Task *entroq.Task
	// Value is the task's value, already unmarshaled into T.
	Value T
	// Sets are the doc sets claimed for this task, in claim order, sets with
	// no docs included. It is nil in TakeDocs, which is where the claim is
	// decided: nothing is held yet, so there is nothing to report. It is
	// filled from CueWork onward.
	Sets []*entroq.DocSet
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
type DoModifyRun[T any] func(context.Context, entroq.Reader, *Work[T]) (*Result, error)

// Result is what a DoModifyRun returns: the modifications the worker applies
// after work completes, plus optional work to run when the task is handled
// successfully. Build it with Modify, then chain OnSuccess for a post-success
// step.
type Result struct {
	mods         []entroq.ModifyArg
	onSuccess    func(context.Context) error
	onDependency func(context.Context, *entroq.DependencyError) error
	stop         bool
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
//
// A result that changes the task resets its claim count (see
// entroq.ResettingClaims): the worker handled it.
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

// ThenStop says this is the last task: commit the result, run any OnSuccess,
// and then end the Run cleanly, with Run returning nil.
//
// It belongs on the result rather than in a hook because the decision is
// already made by the time work ends -- a drain request arrives with the work,
// and a hook would be a callback asking a question whose answer is in hand.
// OnSuccess could not do it in any case: a commit that loses a dependency
// skips it, and a drain should stop either way.
//
// Nothing is abandoned. The modification commits first, so a drain never
// costs the task in hand; cancelling the Run's context would.
func (r *Result) ThenStop() *Result {
	r.stop = true
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

// TakeRun[T] is a function that inspects a newly claimed task and names the
// doc sets the worker must hold to do it, with Take, as a DoModifyRun names
// its commit with Modify:
//
//	func(ctx context.Context, task *entroq.Task, v Order) (*worker.TakeResult, error) {
//		return worker.Take(
//			entroq.ClaimKey("orders", v.Customer),
//			entroq.ClaimKey("stock", v.SKU).WithoutMembers(),
//		), nil
//	}
//
// Returning nil, or naming no sets (or not setting WithTakeDocs), skips the
// acquisition phase. The worker claims every set at once, all or none, as its
// own claimant and until the task's own arrival time, so the task and its
// sets expire together; a set someone else holds leaves nothing claimed, and
// the task is retried with backoff.
type TakeRun[T any] func(context.Context, entroq.Reader, *Work[T]) (*TakeResult, error)

// TakeResult is what a TakeRun returns: the doc sets the worker claims for
// the task before work begins. Build it with Take.
type TakeResult struct {
	args []entroq.DocClaimArg
}

// Take begins a TakeResult that claims the doc sets args name, built with
// entroq.ClaimKey. The worker owns the claim's lease and claimant, as it owns
// the versions of a Modify result: it claims the sets as itself, until the
// task's own arrival time, and ignores any lease (entroq.ClaimingSetsFor,
// entroq.ClaimingSetsUntil) or claimant (entroq.ClaimingSetsAs) in args.
func Take(args ...entroq.DocClaimArg) *TakeResult {
	return &TakeResult{args: args}
}

// CueWorkRun[T] is the WithCueWork function shape: the cue phase. It is given
// the task, its value and the held doc sets, on a context that expires with the
// granted lease (see Handler.CueWork).
type CueWorkRun[T any] func(context.Context, entroq.Reader, *Work[T]) error

// DoRun[T] is the WithDoWork function shape: the work phase. It is given the
// task, its typed value, and any claimed doc sets, but no client -- it runs under
// renewal, so it must not modify the claimed task (see Handler.Finish). Return a
// RetryError/MoveError to retry/move, or any other error to exit.
type DoRun[T any] func(context.Context, entroq.Reader, *Work[T]) error

// FinishRun[T] is the WithFinish function shape: the commit phase. It runs after
// renewal has stopped and is handed its client, so committing the (now stable)
// task is safe. It receives the same value and doc sets as DoRun.
type FinishRun[T any] func(context.Context, entroq.Client, *Work[T]) error

// funcHandler[T] is a Handler[T] backed by plain functions.
type funcHandler[T any] struct {
	take   TakeRun[T]
	cue    CueWorkRun[T]
	do     DoRun[T]
	finish FinishRun[T]
}

// CueWork runs the specified cue function if set, otherwise does nothing.
func (h *funcHandler[T]) CueWork(ctx context.Context, eqc entroq.Reader, r *Work[T]) error {
	if h.cue == nil {
		return nil
	}
	return h.cue(ctx, eqc, r)
}

// TakeDocs runs the specified take function if set, otherwise returns nil.
func (h *funcHandler[T]) TakeDocs(ctx context.Context, eqc entroq.Reader, r *Work[T]) (*TakeResult, error) {
	if h.take == nil {
		return nil, nil
	}
	return h.take(ctx, eqc, r)
}

// DoWork runs the specified "do" function.
func (h *funcHandler[T]) DoWork(ctx context.Context, eqc entroq.Reader, r *Work[T]) error {
	if h.do == nil {
		return FatalErrorf("no work function specified")
	}
	return h.do(ctx, eqc, r)
}

// Finish runs the specified "finish" function if it has been defined.
func (h *funcHandler[T]) Finish(ctx context.Context, eqc entroq.Client, r *Work[T]) error {
	if h.finish == nil {
		return nil
	}
	return h.finish(ctx, eqc, r)
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
	cue      CueWorkRun[T]
	doModify DoModifyRun[T]

	initialTask *entroq.Task
	result      *Result
	released    bool // the commit only made the task arrive
}

func (h *doModifyHandler[T]) TakeDocs(ctx context.Context, eqc entroq.Reader, r *Work[T]) (*TakeResult, error) {
	h.initialTask = r.Task
	if h.take == nil {
		return nil, nil
	}
	return h.take(ctx, eqc, r)
}

// CueWork runs the specified cue function if set, otherwise does nothing.
func (h *doModifyHandler[T]) CueWork(ctx context.Context, eqc entroq.Reader, r *Work[T]) error {
	if h.cue == nil {
		return nil
	}
	return h.cue(ctx, eqc, r)
}

func (h *doModifyHandler[T]) DoWork(ctx context.Context, eqc entroq.Reader, r *Work[T]) error {
	if h.doModify == nil {
		return FatalErrorf("no work function specified")
	}
	result, err := h.doModify(ctx, eqc, r)
	if err != nil {
		return err
	}
	h.result = result
	return nil
}

func (h *doModifyHandler[T]) Finish(ctx context.Context, eqc entroq.Client, r *Work[T]) error {
	finalTask := r.Task
	finalSets := r.Sets
	// initialTask is set unconditionally by TakeDocs, which always runs before
	// Finish, so it is non-nil here by construction.
	//
	// A doc claim is a transaction scoped to this body (see releasing), so the
	// modification that ends the body carries the releases. A body with nothing
	// to commit still ended, and its sets still go back, in a modification of
	// their own.
	modification := entroq.NewModification("")
	if h.result != nil && len(h.result.mods) > 0 {
		if finalTask == nil {
			return FatalErrorf("doModify finish: nil finalized task with modifications to apply")
		}
		if h.initialTask.Version > finalTask.Version {
			return fmt.Errorf("task updated inside worker body, expected version <= %v, got %v", finalTask.Version, h.initialTask.Version)
		}

		modification = entroq.NewModification("", h.result.mods...)
		fixVersions(modification, finalTask, finalSets)
		// The worker changed the task on purpose, so the claims before this
		// one no longer point at a poison pill.
		for _, c := range modification.Changes {
			if c.ID == finalTask.ID {
				modification.ResetClaims(c.ID)
			}
		}
	}
	releasing(modification, finalSets)
	// Modify refuses a modification that names no operation, so a body that
	// decided nothing and held nothing writes nothing.
	if !modification.IsEmpty() {
		if _, err := eqc.Modify(ctx, entroq.WithModification(modification)); err != nil {
			return h.commitFailed(ctx, err)
		}
		if finalTask != nil {
			h.released = onlyArrives(modification, finalTask.ID)
		}
	}

	if h.result == nil {
		// No Result, so nothing to run after the release.
		return nil
	}

	// The task was handled successfully (no error; any modifications committed).
	// OnSuccess is the optimistic post-success step: best-effort (its error is
	// logged), unless it returns a FatalError, which stops the worker. It holds
	// no doc claim: the transaction ended with the commit above.
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
	if fatal == nil && h.result.stop {
		// Everything the result asked for has landed, so stopping now loses
		// nothing. ErrShutdown is what Run already treats as a clean end.
		return fmt.Errorf("stopping after a result that asked to be the last: %w", ErrShutdown)
	}
	return fatal
}

// resultOnDependency returns the handler's dependency hook, or nil when there
// is no Result to hold one.
func (h *doModifyHandler[T]) resultOnDependency() func(context.Context, *entroq.DependencyError) error {
	if h.result == nil {
		return nil
	}
	return h.result.onDependency
}

// commitFailed handles a commit of the handler's result that did not apply.
func (h *doModifyHandler[T]) commitFailed(ctx context.Context, err error) error {
	if depErr, ok := entroq.AsDependency(err); ok {
		log.Printf("Worker ack failed: %v", err)
		// h.result is nil when the commit carried only releases, and there is
		// then no hook to consult.
		if fn := h.resultOnDependency(); fn != nil {
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

// releasing adds to m a release for every doc set claimed here whose arrival m
// has not already decided, so the sets go back in the very transaction that
// ends the worker body.
//
// A doc claim is a transaction scoped to that body: the sets were taken for the
// work, the work is over, so they are no longer in it. They go back whatever
// else m does -- the task need not be in m at all, and a body that mutated docs
// and left the task alone still frees them. Doc sets are shared, so an early
// release unblocks consumers with nothing to do with this task, and the task's
// own arrival says nothing about them.
//
// DECIDED means m names the set in an arrival, or a member write asks to hold it
// past now (By above zero, which is docset's own rule for what holds a set).
// Explicit intent wins: a handler that pushes a set into the future keeps it.
// Expected to be rare -- doing nothing releases everything, which is the useful
// default. A delete never holds a set, and depending on a doc only watches it,
// so neither keeps one.
//
// The result only ever names sets claimed here, and must stay that way:
// releasing moves a set's version, so naming one this worker holds no lease on
// would disturb a set that is somebody else's or nobody's. The leasehold
// decides what MAY be released and m only decides what to leave out.
//
// Nothing releases on a path where the body FAILED -- a dependency error on the
// commit, a fatal error, a panic. There is nothing to clean up: a set claimed
// with MatchingLeaseOf took the task's own arrival, read in one transaction, and
// renewal keeps the two equal, so when nothing moved the task and its sets lapse
// together and no cleanup could beat the lease.
func releasing(m *entroq.Modification, sets []*entroq.DocSet) {
	if len(sets) == 0 {
		return
	}
	decided := make(map[setKey]bool)
	for _, a := range m.DocArrives {
		decided[setKey{a.Namespace, a.Key}] = true
	}
	for _, ins := range m.DocInserts {
		if ins.By() > 0 {
			decided[setKey{ins.Namespace, ins.Key}] = true
		}
	}
	for _, d := range m.DocChanges {
		if d.By() > 0 {
			decided[setKey{d.Namespace, d.Key}] = true
		}
	}
	var free []*entroq.DocSet
	for _, g := range sets {
		if !decided[setKey{g.Namespace, g.Key}] {
			free = append(free, g)
		}
	}
	entroq.Arriving(entroq.ReadyNow().Docs(free...))(m)
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
	// eqc is the connection, whose own claimant no Run uses: Run scopes it
	// (entroq.Client.As) so each consumer holds what it claims as itself.
	// Nothing in the per-task path may reach for this -- it has the wrong
	// claimant -- so it appears only in Run, and runs carry the scoped client.
	eqc entroq.Client

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
	// runSeq names consumers apart within this worker, under mu.
	runSeq int
}

// activeRun holds what Shutdown needs to stop one Run, and the consumer that
// Run holds everything as.
type activeRun struct {
	cancel      context.CancelFunc // ends the Run, canceling its handler
	cancelClaim context.CancelFunc // ends its claim; nil when not claiming
	// eqc is this Run's consumer: the client scoped to a claimant no other Run
	// shares. Everything in the per-task path goes through it.
	eqc entroq.Client
}

// ErrShutdown is returned by Run on a worker that Shutdown has been called on.
var ErrShutdown = errors.New("worker: shut down")

// workerOpts holds built-up worker options to be later checked against as a
// new worker is created.
type workerOpts[T any] struct {
	makeHandler MakeHandler[T]

	// These are all potential inputs to create the default handler.
	take     TakeRun[T]
	cue      CueWorkRun[T]
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
func New[T any](eq entroq.Client, opts ...Option[T]) *Worker[T] {
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
	// DoModify handlers win. TakeDocs and CueWork are always used.
	if wOpts.doModify != nil {
		worker.makeHandler = func() (Handler[T], error) {
			return &doModifyHandler[T]{
				take:     wOpts.take,
				cue:      wOpts.cue,
				doModify: wOpts.doModify,
			}, nil
		}
	} else {
		worker.makeHandler = func() (Handler[T], error) {
			return &funcHandler[T]{
				take:   wOpts.take,
				cue:    wOpts.cue,
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
// needed, with Take (see TakeRun). The worker claims them all at once, and a
// set claimed by another worker causes a backoff-and-retry with none held. A
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

// WithCueWork sets the cue function: once the task's doc sets are held and
// before renewal begins, it tells whoever will do the work that work is
// coming, so that it can be ready (see CueWorkRun and Handler.CueWork).
//
// Nothing renews the claim while it runs, so a cue that never completes blocks
// this worker and lets the task's lease lapse into someone else's hands. That
// is the point: reaching into another process to hand work over belongs here,
// and not in DoWork, where renewal would pin the task for as long as nobody
// answered.
//
// When used with WithMakeHandler, the handler's CueWork method takes
// precedence and WithCueWork has no effect.
func WithCueWork[T any](f CueWorkRun[T]) Option[T] {
	return func(wo *workerOpts[T]) {
		wo.cue = f
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
//   - WithCueWork - tells whoever does the work that it is coming, before the renewal clock starts.
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
// released in that same modification -- see releasing. Riding along rather than
// following it leaves no window where the task is available again and its docs
// are not, which a worker claiming the retried task would otherwise lose a
// backoff to.
//
// This applies to both handler paths. A sentinel skips Finish entirely, so the
// worker is the one writing this modification whichever handler asked for it,
// and nothing here second-guesses a hand-written Finish.
func (w *Worker[T]) handleSentinelErrors(ctx context.Context, eqc entroq.Client, sentinel error, task *entroq.Task, sets []*entroq.DocSet, errQ string, opts *runOpt) (isSentinel bool, err error) {
	if re, ok := AsRetry(sentinel); ok {
		delay := opts.baseRetryDelay
		if re.hasAfter {
			delay = re.after
		}
		q := errQ
		if re.moveTo != "" {
			q = re.moveTo
		}
		if _, err := eqc.Modify(ctx,
			task.RetryOrQuarantine(re.Error(), q, opts.maxAttempts, entroq.ArrivalTimeBy(delay)),
			entroq.Arriving(entroq.ReadyNow().Docs(sets...))); err != nil {
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
		if _, err := eqc.Modify(ctx, task.Quarantine(me.Error(), q),
			entroq.Arriving(entroq.ReadyNow().Docs(sets...))); err != nil {
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

// acquireDocs performs the doc acquisition phase for a claimed task: it
// claims the doc sets the handler's TakeDocs named in tr.
//
// It returns the claimed sets in claim order, sets with no docs included.
//
// Returns a *entroq.DependencyError while another claimant holds a set, and
// also when this worker's own lease on the task has already run out, since the
// hold comes from the task. The caller retries the task with backoff either
// way; the error says which it was.
func acquireDocs(ctx context.Context, eqc entroq.Client, task *entroq.Task, tr *TakeResult) ([]*entroq.DocSet, error) {
	if tr == nil {
		return []*entroq.DocSet{}, nil
	}
	// Only the sets are the handler's; the lease and claimant are the
	// worker's.
	sets := entroq.NewDocClaim(tr.args...).Sets
	if len(sets) == 0 {
		return []*entroq.DocSet{}, nil
	}

	// One claim of every set, all or none, so contention leaves nothing
	// held. The sets are held until the task's own arrival, so that if the
	// worker dies they come free with it.
	sort.Slice(sets, func(i, j int) bool {
		if sets[i].Namespace != sets[j].Namespace {
			return sets[i].Namespace < sets[j].Namespace
		}
		return sets[i].Key < sets[j].Key
	})
	args := make([]entroq.DocClaimArg, 0, len(sets)+1)
	for _, s := range sets {
		args = append(args, s)
	}
	// The sets are held until the task itself arrives, named rather than
	// timed: the backend reads the task, so the two expire at exactly one
	// instant instead of approximately, and a task this worker no longer holds
	// fails the claim rather than locking sets to a lease nobody owns.
	args = append(args, entroq.MatchingLeaseOf(task))
	return eqc.ClaimDocs(ctx, args...) // caller inspects DependencyError
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
	task, err := run.eqc.Claim(claimCtx, entroq.From(opts.qs...), entroq.ClaimFor(opts.lease))
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
	defer func() { w.metrics.recordTask(ctx, task.Queue, run.eqc.ID(), outcome) }()

	if opts.maxClaims > 0 && task.Claims > opts.maxClaims {
		errQ := w.ErrorQueueFor(task.Queue)
		if _, err := w.handleSentinelErrors(ctx, run.eqc,
			MoveErrorf("maximum claims exceeded: %d claims without modification (limit %d, lease %v)",
				task.Claims, opts.maxClaims, opts.lease), task, nil, errQ, opts,
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
		// A value that does not decode is a poison pill: no claim of it will
		// do better. Move it to the error queue, saying why, and go on.
		outcome = outcomeMoved
		move := MoveErrorf("value does not decode as %T: %v", value, err)
		if _, herr := w.handleSentinelErrors(ctx, run.eqc, move, task, nil, w.ErrorQueueFor(task.Queue), opts); herr != nil {
			return fmt.Errorf("worker (%q) move undecodable task: %w", opts.qs, herr)
		}
		return nil
	}

	// Phase 2: Acquire docs before renewal starts. Doc claims are sorted by
	// (namespace, key) to prevent dining-philosopher livelock when multiple
	// doc sets are acquired.
	// One Work for the whole task, so a phase sees what the last one left:
	// Sets is filled in after the claim, and the renewal handoff replaces both
	// the task and the sets before the commit phase.
	work := &Work[T]{Task: task, Value: value}
	tr, err := handler.TakeDocs(rCtx, run.eqc, work)
	if err != nil {
		// A sentinel acts on the task as it does from DoWork; nothing is
		// claimed yet.
		if isSentinelError(err) {
			outcome = sentinelOutcome(err)
			_, serr := w.handleSentinelErrors(ctx, run.eqc, err, task, nil, w.ErrorQueueFor(task.Queue), opts)
			return serr
		}
		if w.quarantineAtLimit(ctx, run.eqc, task, nil, err, opts) {
			outcome = outcomeMoved
		}
		return fmt.Errorf("take docs: %w", err)
	}
	sets, err := acquireDocs(rCtx, run.eqc, task, tr)
	if err != nil {
		// Two transient failures, one outcome: the task goes back with a
		// delay. The claim is all or none, so nothing is held either way.
		//
		// They are recorded apart because they blame different things. A set
		// held by someone else is contention, and the error names the sets. A
		// failed depend on the task itself means this worker ran past its own
		// lease while taking docs, so the hold it asked for was already
		// behind it -- and the retry below may well find the task gone, which
		// handleSentinelErrors treats as the known state it is.
		if depErr, ok := entroq.AsDependency(err); ok {
			outcome = outcomeRetried
			retry := RetryErrorf("doc contention: %v", depErr)
			if depErr.HasMissing() {
				retry = RetryErrorf("task lease lapsed while taking docs: %v", depErr)
			}
			errQ := w.ErrorQueueFor(task.Queue)
			if _, herr := w.handleSentinelErrors(ctx, run.eqc, retry.After(opts.contentionDelay()), task, nil, errQ, opts); herr != nil {
				return fmt.Errorf("handle sentinel error: %w", herr)
			}
			return nil
		}
		return fmt.Errorf("acquire docs: %w", err)
	}

	// Phase 3: CueWork, the last phase before anything renews the claim. The
	// sets are attached first, so the cue names what is held and the same Work
	// carries them on into DoWork.
	//
	// A cue that never completes blocks this worker, and that is the
	// deliberate shape: nothing is renewing, so the task's lease runs out and
	// another worker picks it up. Giving up on a schedule instead would be
	// worse than blocking -- each abandoned cue costs the task a claim, and a
	// worker whose far end has gone away would walk the queue quarantining
	// tasks at the claim limit for a fault that is not theirs.
	work.Sets = sets
	if err := handler.CueWork(rCtx, run.eqc, work); err != nil {
		// Pre-renewal, exactly as TakeDocs: a sentinel acts on the task, and
		// anything else stops the worker after a best-effort quarantine if
		// this claim was the last the limit allows.
		if isSentinelError(err) {
			outcome = sentinelOutcome(err)
			_, serr := w.handleSentinelErrors(ctx, run.eqc, err, task, sets, w.ErrorQueueFor(task.Queue), opts)
			return serr
		}
		if w.quarantineAtLimit(ctx, run.eqc, task, sets, err, opts) {
			outcome = outcomeMoved
		}
		return fmt.Errorf("cue work: %w", err)
	}

	// Phase 4: DoWork with background renewal of task + docs together.
	var (
		sentinelErr error
		workErr     error // the handler's own error, other than a sentinel
		finalTask   *entroq.Task
		finalSets   []*entroq.DocSet
	)

	// WithWorkTimeout bounds the body by bounding the context it and the
	// renewal share: the body is asked to stop, and renewal stops with it
	// rather than holding a task the worker has given up on. The commit path
	// below uses ctx, not this one, so a disposition can still be written.
	workCtx := rCtx
	cancelWork := context.CancelFunc(func() {})
	if opts.workTimeout > 0 {
		workCtx, cancelWork = context.WithTimeout(rCtx, opts.workTimeout)
	}
	defer cancelWork()

	// Only a duration crosses into doWhileRenewing: how long the claim has
	// already been running locally. It derives both the hold it renews for and
	// the cadence from the task, so opts.lease -- what the claim ASKED for --
	// does not follow it in and cannot be renewed on by mistake.
	renewErr, handleErr := doWhileRenewing(workCtx, run.eqc, time.Since(claimed), held{task: task, sets: sets},
		func() { w.metrics.recordRenewalRetry(ctx, task.Queue, run.eqc.ID()) },
		func(ctx context.Context, stop finalizeRenew) error {
			defer func() {
				final := stop()
				finalTask = final.task
				finalSets = final.sets
			}()
			if err := handler.DoWork(ctx, run.eqc, work); err != nil {
				if !isSentinelError(err) {
					workErr = err
					return fmt.Errorf("task do: %w", err)
				}
				sentinelErr = err
			}
			return nil
		},
	)

	// Once renewal has stopped, that is why the work ended, whatever the
	// handler returned, except that a handler's FatalError still stops the
	// worker: handlers keep that control.
	//
	// Renewal stopping is a lost claim, however it happened -- refused, or
	// answered in a way that leaves the hold unnameable (held.lostClaimErrorf). That
	// ends this task, not the worker: the task is someone else's now, and
	// there is nothing to commit, so a retry or move could not land either.
	// What remains here is a transport or context failure, which is the
	// deployment's to fix.
	if renewErr != nil {
		if fe, ok := AsFatal(sentinelErr); ok {
			return fe
		}
		if _, ok := entroq.AsDependency(renewErr); ok {
			log.Printf("worker (%q): claim of task %s lost, going on: %v", opts.qs, task.ID, renewErr)
			outcome = outcomeLost
			return nil
		}
		return fmt.Errorf("worker (%q) renewal: %w", opts.qs, renewErr)
	}

	if sentinelErr != nil {
		outcome = sentinelOutcome(sentinelErr)
		errQ := w.ErrorQueueFor(task.Queue)
		if _, err := w.handleSentinelErrors(ctx, run.eqc, sentinelErr, finalTask, finalSets, errQ, opts); err != nil {
			return fmt.Errorf("handle sentinel error: %w", err)
		}
		return nil
	}

	if handleErr != nil {
		// A body that ran past WithWorkTimeout failed for a reason that is
		// neither the handler's nor the deployment's, so the task goes back
		// with its attempt counted rather than stopping the worker. Checked
		// after sentinelErr, so a body that returns a sentinel on its way out
		// is still taken at its word.
		//
		// A body that finished in time is not second-guessed: the deadline
		// only decides why a body that FAILED did so.
		if opts.workTimeout > 0 && errors.Is(workCtx.Err(), context.DeadlineExceeded) {
			outcome = outcomeRetried
			errQ := w.ErrorQueueFor(task.Queue)
			retry := RetryErrorf("work did not finish within %v: %v", opts.workTimeout, handleErr)
			if _, err := w.handleSentinelErrors(ctx, run.eqc, retry, finalTask, finalSets, errQ, opts); err != nil {
				return fmt.Errorf("handle work timeout: %w", err)
			}
			return nil
		}
		if workErr != nil && w.quarantineAtLimit(ctx, run.eqc, finalTask, finalSets, workErr, opts) {
			outcome = outcomeMoved
		}
		return fmt.Errorf("worker (%q): %w", opts.qs, handleErr)
	}

	// Phase 5: Finish with stable versions — renewal has stopped.
	work.Task = finalTask
	work.Sets = finalSets
	if err := handler.Finish(ctx, run.eqc, work); err != nil {
		// A result that asked to be the last (Result.ThenStop) reports a clean
		// end once its commit has landed, so the task counts as done and Run
		// returns nil. Checked before the ladder below, which is for failures.
		if errors.Is(err, ErrShutdown) {
			outcome = outcomeDone
			if h, ok := handler.(*doModifyHandler[T]); ok && h.released {
				outcome = outcomeReleased
			}
			return err
		}
		// A post-commit hook (OnDependency) may return a Retry/Move/Fatal
		// sentinel; route it through the same machinery as a work-phase sentinel
		// before falling back to the default dependency reclaim.
		errQ := w.ErrorQueueFor(task.Queue)
		if isSentinel, serr := w.handleSentinelErrors(ctx, run.eqc, err, finalTask, finalSets, errQ, opts); isSentinel {
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

// atLimitTimeout bounds quarantineAtLimit's attempt, which must not hold up a
// worker that is about to stop.
const atLimitTimeout = 5 * time.Second

// quarantineAtLimit moves task to its error queue at once, recording err,
// which its handler returned (not a sentinel), when this claim was the last
// the claim limit allows: the next claim would move it
// anyway, without saying why. Below the limit it records nothing, since a
// record is a modification and would reset the claim count; the task waits
// out its lease, as before.
//
// The attempt is best-effort, with its own short timeout, apart from a
// shutdown in progress, and the worker stops afterward exactly as it would
// have without it: nothing is swallowed. It reports whether the task moved.
func (w *Worker[T]) quarantineAtLimit(ctx context.Context, eqc entroq.Client, task *entroq.Task, sets []*entroq.DocSet, err error, opts *runOpt) bool {
	if opts.maxClaims <= 0 || task == nil || task.Claims < opts.maxClaims {
		return false
	}
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), atLimitTimeout)
	defer cancel()
	move := MoveErrorf("handler failed at the claim limit (%d claims, limit %d): %v", task.Claims, opts.maxClaims, err)
	if _, qerr := w.handleSentinelErrors(qctx, eqc, move, task, sets, w.ErrorQueueFor(task.Queue), opts); qerr != nil {
		log.Printf("worker: quarantine of task %s at its claim limit: %v", task.ID, qerr)
		return false
	}
	return true
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
	qs                 []string
	baseRetryDelay     time.Duration
	docContentionDelay time.Duration // 0: baseRetryDelay
	maxAttempts        int32
	maxClaims          int32
	workTimeout        time.Duration
	claimant           string
	lease              time.Duration
}

// Watching specifies the queues Run will watch.
func Watching(qs ...string) RunOption {
	return func(ro *runOpt) {
		ro.qs = qs
	}
}

// WithLease sets the lease a Run claims for, which also sets the frequency of
// renewal: a hold is renewed two thirds of the way through what the claim
// granted. There is no minimum. Whether a renewal arrives before the server's
// clock runs the lease out is the server's to judge, and a renewal that does
// not arrive in time loses the claim, which is a designed outcome rather than
// a failure: the task returns to its queue and another worker takes it. A
// service clamps a lease it considers too short, so what is left here is a
// caller's own process holding its own claims.
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

// AsClaimant names the consumer this Run holds everything as, instead of the
// "<connection ID>/<n>" it would pick.
//
// Every Run is its own consumer either way; this only chooses the name. Use it
// for a name that survives a restart, so metrics series and anything reading
// claimants off stored tasks stay recognizable -- a pod name, say. Two
// concurrent Runs given the SAME name are one consumer again, with the doc-set
// exclusion that implies, so a name has to be as distinct as the Run is.
func AsClaimant(id string) RunOption {
	return func(ro *runOpt) {
		ro.claimant = id
	}
}

// WithWorkTimeout bounds how long a handler body may run, overriding
// DefaultWorkTimeout. Pass ZERO for no bound at all, which is the only way to
// say that a body may run as long as it likes.
//
// A body that runs past the bound has its context canceled -- which is how it
// is asked to stop, so a body that never checks its context cannot be made to
// -- and the task is RETRIED rather than quarantined, with its attempt
// counted. A hang that was bad luck comes back and succeeds; a task that always
// hangs exhausts WithMaxAttempts and is quarantined for inspection, instead of
// wedging a worker on every claim.
//
// Renewal stops at the deadline too, so an abandoned body is not holding the
// task past the moment the worker gave up on it.
//
// The bound and the lease are separate on purpose. Before background renewal
// existed they were one thing and a body simply had to fit inside a lease;
// renewal untied them, and then the ability to say how long work should take
// was lost with it. This is that knob back: the lease governs how quickly a
// lost task is recovered, this governs how long work is allowed to take.
func WithWorkTimeout(d time.Duration) RunOption {
	return func(ro *runOpt) {
		ro.workTimeout = d
	}
}

// WithMaxClaims sets the maximum number of times a task may be claimed without
// being modified before it is moved to the worker's error queue without
// constructing or invoking the handler. If 0 (the default), there is no
// maximum.
//
// The worker resets a task's claim count whenever it modifies the task (see
// entroq.ResettingClaims), including a retry, and renewal keeps it. So the count
// is of claims the task did not survive: a handler that crashed, hung, or
// needed longer than the lease. A task that reaches the limit is a poison
// pill, or runs under too short a lease; the error it is moved with gives the
// count, the limit, and the lease.
//
// On a task's last allowed claim, a handler that returns an error other than
// a sentinel moves it to the error queue at once, with that error, before
// the worker stops: the next claim would have moved it without saying why.
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

// WithDocContentionDelay sets the delay before a task is retried because a
// doc set it needs is held by someone else; by default it is the base retry
// delay. Up to a quarter more is added at random, so tasks that lost to the
// same holder do not all come back at once and collide again. The retry
// counts as an attempt: contention is the task's intent failing, which
// should be rare, and repeated contention points at a design that lets many
// owners want the same set.
func WithDocContentionDelay(d time.Duration) RunOption {
	return func(ro *runOpt) {
		ro.docContentionDelay = d
	}
}

// contentionDelay returns the delay before a retry after doc contention:
// the configured delay, or the base retry delay, plus up to a quarter more
// at random.
func (ro *runOpt) contentionDelay() time.Duration {
	d := ro.docContentionDelay
	if d <= 0 {
		d = ro.baseRetryDelay
	}
	if d <= 0 {
		return d
	}
	return jitterLater(d, d/4)
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
//     inspect and redirect this case. A claim lost while the handler works
//     (renewal finds the task or a set no longer ours) is the same: the task is
//     someone else's, it is logged and counted as lost, and the loop continues,
//     whatever the handler returned once its context was canceled, except a
//     FatalError, which still stops the worker.
//   - A value that does not decode into T: a poison pill. The task moves to
//     its error queue, saying why, and the loop continues.
//   - Context cancellation or timeout: a clean stop; Run returns nil. Shutdown
//     stops Run the same way, after its task is done.
//   - Anything else: the worker exits. An unclassified error leaves the loop in an
//     unknown state, and crashing for an orchestrator to restart is safer, and
//     louder and thus more fixable, than continuing from state neither the author
//     nor the framework reasoned about. If the handler returned it on the task's
//     last claim under WithMaxClaims, the task is moved to its error queue with
//     the error first. Classify the failures you understand as
//     Retry/Move so only genuine surprises bring the worker down.
//
// A claimant is a consumer, not a process, so every Run is its own consumer.
// Run scopes the worker's client to a claimant of its own (entroq.Client.As),
// which is what lets concurrent Runs -- of one worker, or of workers sharing a
// connection -- hold doc sets without claiming each other's. Nothing has to be
// arranged for that; AsClaimant only renames it.
//
// The claimant is "<connection ID>/<n>" by default, which is stable for the
// Run's life and visibly its connection's. A deployment wanting durable names
// across restarts, for metrics series that survive one, passes AsClaimant.
func (w *Worker[T]) Run(ctx context.Context, opts ...RunOption) error {
	ro := &runOpt{
		lease:          entroq.DefaultClaimDuration,
		baseRetryDelay: DefaultRetryDelay,
		workTimeout:    DefaultWorkTimeout,
	}
	for _, opt := range opts {
		opt(ro)
	}

	if len(ro.qs) == 0 {
		return fmt.Errorf("no queues specified to work on")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	run := &activeRun{cancel: cancel, eqc: w.newClient(ro.claimant)}
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

// newClient returns a view on the "real" EntroQ client that operates with a
// new claimant ID so it's safe to use in a Run function without colliding with
// other Runs on the same worker.
func (w *Worker[T]) newClient(claimant string) entroq.Client {
	if claimant == "" {
		w.mu.Lock()
		w.runSeq++
		claimant = fmt.Sprintf("%s/%d", w.eqc.ID(), w.runSeq)
		w.mu.Unlock()
	}
	return w.eqc.As(claimant)
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
	e.after = d
	e.hasAfter = true
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
		return held{}, h.lostClaimErrorf("renewal of task %s answered with tasks %v", h.task.ID, resp.ChangedTasks)
	}
	locks := make(map[setKey]*entroq.DocSet, len(resp.ChangedSets))
	for _, l := range resp.ChangedSets {
		locks[setKey{l.Namespace, l.Key}] = l
	}
	out := held{task: resp.ChangedTasks[0], sets: make([]*entroq.DocSet, 0, len(h.sets))}
	for _, g := range h.sets {
		l, ok := locks[setKey{g.Namespace, g.Key}]
		if !ok {
			return held{}, h.lostClaimErrorf("renewal of task %s did not answer with doc set %q in %q", h.task.ID, g.Key, g.Namespace)
		}
		out.sets = append(out.sets, relocked(g, l))
	}
	return out, nil
}

// lostClaimErrorf reports that the hold is gone, as a failed depend on the task.
//
// A renewal answering without naming everything it renewed leaves a version
// the worker cannot name again, and a worker that cannot name its hold does
// not have one. That is the same verdict a refused renewal reaches, so it is
// the same error: the task goes back and this worker takes another, rather
// than the process ending over a reply it could not read.
func (h held) lostClaimErrorf(format string, args ...any) error {
	depErr := entroq.DependencyErrorf("%s", fmt.Sprintf(format, args...))
	depErr.Depends = append(depErr.Depends, h.task.IDVersion())
	return depErr
}

// grantedLease returns the lease a claim or renewal actually GRANTED, which a
// service may have clamped above or below what was asked for.
//
// At minus Modified: both are stamped from one server clock, so the difference
// is exact, immune to network latency and to clock disagreement.
// eqtest.ClaimStampsLease holds every backend to that pairing.
//
// Only the task is worth measuring: its doc sets are claimed until its own
// arrival and a clamp can only move those later, so the task expires first.
func grantedLease(t *entroq.Task) time.Duration {
	return max(0, t.At.Sub(t.Modified))
}

// renewInterval returns how long after a claim, and after each renewal, a hold
// should be renewed: part of the granted lease, leaving the rest as margin for
// the renewal to complete in. Going by the REQUESTED lease would first renew
// after a clamped hold had already expired.
func renewInterval(t *entroq.Task) time.Duration {
	return entroq.RenewalDurationFor(grantedLease(t))
}

// renewalMargin returns how long a renewal has to complete in: the part of the
// granted lease the cadence deliberately leaves unused. A renewal that fails
// transiently has this long to succeed on a later try before the lease it is
// extending runs out.
func renewalMargin(t *entroq.Task) time.Duration {
	return grantedLease(t) - renewInterval(t)
}

// nextRenewalRetry returns how long to wait before retrying a renewal that
// failed transiently, given the wait before it (zero when the last renewal
// succeeded).
//
// Waiting the ordinary interval would spend the whole margin on one more
// attempt, so a single dropped packet costs the claim. The first retry comes
// at an eighth of the margin instead, and each consecutive failure doubles it:
// a blip is recovered from quickly, and three attempts still fit inside the
// margin at any lease length, without a tuned constant to pick.
//
// It stops growing at the ordinary interval, because past that a retry is no
// faster than simply renewing, and an outage lasting minutes should cost
// renewals at the ordinary rate rather than several times it. A margin that is
// not positive leaves nothing to retry inside, so the interval is all there
// is.
func nextRenewalRetry(last, margin, interval time.Duration) time.Duration {
	if margin <= 0 {
		return interval
	}
	next := margin / 8
	if last > 0 {
		next = last * 2
	}
	return min(next, interval)
}

// relocked returns g, and each of its docs, at lock l.
func relocked(g, l *entroq.DocSet) *entroq.DocSet {
	ng := *g
	ng.Version = l.Version
	ng.Claimant = l.Claimant
	ng.At = l.At
	ng.NumDocs = l.NumDocs
	ng.Docs = make([]*entroq.Doc, 0, len(g.Docs))
	for _, d := range g.Docs {
		nd := *d
		nd.Version = l.Version
		nd.Claimant = l.Claimant
		nd.At = l.At
		ng.Docs = append(ng.Docs, &nd)
	}
	return &ng
}

// finalizeRenew stops renewal from a worker routine and returns what it held,
// at the stable versions it left them at.
type finalizeRenew func() held

// workFn handles tasks and docs while renewal runs in the background.
type workFn func(ctx context.Context, stop finalizeRenew) error

// doWhileRenewing runs work while keeping h claimed in the background: every
// interval, one UpdateArrival renews the task and every set for the lease the
// claim was granted, sets with no docs included. A renewal that finds the
// claim lost, a server that cannot renew, or a renewal that does not return
// what it renewed cancels work with that error as its cause, and returns it as
// renewErr, whatever the work then returned: once renewal has stopped, that is
// why the work ended.
//
// Both the hold and the cadence come from the granted lease, At minus
// Modified, and never from the lease the claim asked for: a service may clamp
// a request, and renewing for the request would then hold the task for a
// different span than the one the cadence was computed from. Renewing for what
// is held keeps every grant equal to the last, which is why computing the
// cadence once is not merely simpler but exact.
//
// A renewal that fails comes back on the same schedule rather than any sooner
// for having failed: losing the claim is a designed outcome, and retrying a
// struggling server faster is the wrong direction.
//
// sinceClaim is how long the claim has already been running when renewal
// starts, which is not zero: the doc sets were claimed in between, and the
// lease was running throughout. Only the first wait is shortened by it, to
// nothing at all if claiming the sets outlasted a whole interval. It arrives
// as a duration rather than an instant on purpose, so that no clock reading
// crosses into here and the local and server clocks never meet.
func doWhileRenewing(ctx context.Context, c entroq.Client, sinceClaim time.Duration, h held, onRetry func(), work workFn) (renewErr, err error) {
	if h.task == nil {
		return nil, fmt.Errorf("do while renewing: nothing to renew")
	}
	granted := grantedLease(h.task)
	interval := renewInterval(h.task)
	margin := renewalMargin(h.task)
	type outVal struct {
		held held
		err  error
	}
	taskCh := make(chan outVal, 1)

	g, ctx := errgroup.WithContext(ctx)

	fctx, fcancel := context.WithCancelCause(ctx)
	defer fcancel(nil)

	// stopErr is why renewal stopped, if it did; only the renewal goroutine
	// writes it, and it is read after both have finished.
	var stopErr error
	stopRenew := make(chan struct{})
	g.Go(func() error {
		cur := h
		next := max(0, interval-sinceClaim)
		// retryDelay is the wait before the last retry of a failed renewal,
		// zero while renewals are succeeding.
		var retryDelay time.Duration
		var out chan<- outVal
		stop := func(err error) {
			fcancel(err)
			stopErr = err
			out = taskCh
		}
		doneCh := ctx.Done()
		for {
			// A task whose arrival is not after its modification was never
			// held, so there is no lease to extend and no cadence to do it on.
			// A nil channel never fires, which parks the renewal rather than
			// waking it continuously on a zero timer.
			var tick <-chan time.Time
			if interval > 0 {
				tick = time.After(next)
			}
			select {
			case <-stopRenew:
				out = taskCh
				stopRenew = nil
			case <-doneCh:
				out = taskCh
				doneCh = nil
			case <-tick:
				// Leave the first interval behind, which may have been short or
				// zero because the claim's lease had already run, and settle
				// onto the steady cadence.
				next = interval
				if stopErr != nil {
					break
				}
				resp, err := c.UpdateArrival(ctx, entroq.ReadyIn(granted).Tasks(cur.task).Docs(cur.sets...))
				_, lost := entroq.AsDependency(err)
				switch {
				case err == nil:
					renewedHeld, err := cur.renewed(resp)
					if err != nil {
						stop(err)
						break
					}
					cur = renewedHeld
					retryDelay = 0
				case entroq.IsCanceled(err):
					out = taskCh
				case lost || entroq.IsUnsupported(err):
					// The claim is gone, or the server cannot renew it:
					// nothing holds the work any longer.
					stop(err)
				default:
					// Retry inside the margin instead of after another full
					// interval, which would spend the whole margin on one
					// attempt. Jittered later because a server that failed
					// this renewal failed every worker's at once.
					retryDelay = nextRenewalRetry(retryDelay, margin, interval)
					next = jitterLater(retryDelay, retryDelay/4)
					if onRetry != nil {
						onRetry()
					}
					log.Printf("Transient renewal error, retrying in %v: %v", next, err)
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
		return stopErr, fmt.Errorf("do with renew all: %w", err)
	}
	return stopErr, nil
}
