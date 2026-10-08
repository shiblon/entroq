package eqmr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
	"golang.org/x/sync/errgroup"
)

// mapDone is the finish task a mapper commits when its split is processed. Runs
// are durable but unpublished: only the controller decides whether this attempt
// is the one that counts, and it either publishes them or collects them.
type mapDone struct {
	// SplitKey is the split doc's key, which the controller claims, and Split
	// identifies the doc the mapper read, so a finish for a split that was
	// already accepted is recognized as a loser.
	SplitKey string            `json:"split_key"`
	Split    entroq.DocID      `json:"split"`
	Runs     []intermediateRun `json:"runs"`
}

// reduceDone is the finish task a reducer commits when its partition is
// reduced. It carries a reference to the staged output, not the output, and
// the pointers and runs it consumed, so the controller can commit the output
// and retire the inputs in one step.
type reduceDone struct {
	Partition int               `json:"partition"`
	Pointers  []entroq.DocID    `json:"pointers"`
	Runs      []intermediateRun `json:"runs"`
	Sink      Descriptor        `json:"sink"`
	Staged    json.RawMessage   `json:"staged"`
}

// barrierCheck is the value of a check task: a request to see whether a phase
// is complete. It carries nothing, because what it checks is the run's docs as
// they stand when it is handled.
type barrierCheck struct{}

// gcTask asks for intermediate data to be deleted. That is store I/O of
// unbounded latency, so it is its own unit of work rather than something a
// controller handler does while it holds a claim.
//
// Runs are deleted at once, because each was retired by a decision whose
// evidence sat inside one EntroQ commit: a losing map finish, or a reduced
// partition's consumed inputs. Staged output is never deleted that way, since
// committing it happened outside EntroQ; instead, once the run has ended,
// Stores and Sinks are swept of everything they hold for it that nothing
// committed, which also collects what crashed workers wrote and never
// reported.
type gcTask struct {
	Runs   []intermediateRun `json:"runs,omitempty"`
	Stores []Descriptor      `json:"stores,omitempty"`
	Sinks  []Descriptor      `json:"sinks,omitempty"`
}

// RunController runs every controller worker for this run until ctx ends or
// one of them exits.
//
// The controller is a set of workers, each watching one queue. Accepting a
// unit of work claims only that unit's input, so acceptances of different
// units never wait on one another. Each acceptance commits a check task, and
// only a check that finds its phase complete claims the run's state doc, so
// the run as a whole is serialized only at phase boundaries. Run as many
// replicas as you like; one that dies loses nothing, since its tasks wait in
// their queues and its claims expire.
//
// A controller that cannot reach the store or sink an existing run names fails
// that run at once, with a reason naming what is missing, and returns an
// error: it could never complete the run, and neither could a resubmission
// until the deployment is fixed.
func (c *Controller) RunController(ctx context.Context) error {
	if err := c.checkGrants(ctx); err != nil {
		return err
	}
	g, gctx := errgroup.WithContext(ctx)
	for _, run := range c.controllerRoles() {
		g.Go(func() error { return run(gctx) })
	}
	return g.Wait()
}

// controllerRoles is each controller worker bound to its queue, so RunController
// and the single-process Run helper start exactly the same set.
func (c *Controller) controllerRoles() []func(context.Context) error {
	return []func(context.Context) error{
		func(ctx context.Context) error {
			return c.mapDoneWorker().Run(ctx, c.controlRunOptions(c.MapDoneQ())...)
		},
		func(ctx context.Context) error {
			return c.mapCheckWorker().Run(ctx, c.controlRunOptions(c.MapCheckQ())...)
		},
		func(ctx context.Context) error {
			return c.reduceDoneWorker().Run(ctx, c.controlRunOptions(c.ReduceDoneQ())...)
		},
		func(ctx context.Context) error {
			return c.reduceCheckWorker().Run(ctx, c.controlRunOptions(c.ReduceCheckQ())...)
		},
		func(ctx context.Context) error {
			return c.gcWorker().Run(ctx, c.gcRunOptions()...)
		},
		func(ctx context.Context) error {
			return c.errWorker().Run(ctx, c.controlRunOptions(c.errQueues()...)...)
		},
	}
}

// controlRunOptions configures a controller worker's run.
//
// Each run has its own claimant, the worker default, which is what makes a
// doc claim exclude the other controller workers sharing this client. Finding
// a doc held is rare by construction, and the holder is about to commit and
// release it, so the task is retried at once rather than after the worker
// default. There is no claim ceiling for the same reason.
func (c *Controller) controlRunOptions(queues ...string) []worker.RunOption {
	return []worker.RunOption{
		worker.Watching(queues...),
		worker.WithLease(c.cfg.Lease),
		worker.WithBaseRetryDelay(0),
	}
}

// Collection retry policy. A store that is briefly unavailable gets a few
// chances, spaced out; one that stays unavailable has its task quarantined,
// where the error worker keeps it for an operator without failing the run.
const (
	gcMaxAttempts = 10
	gcRetryDelay  = time.Second
)

// gcRunOptions configures the collector's run. It takes no doc claims, so it
// shares none of the controller's contention policy.
func (c *Controller) gcRunOptions() []worker.RunOption {
	opts := []worker.RunOption{
		worker.Watching(c.GCQ()),
		worker.WithLease(c.cfg.Lease),
		worker.WithMaxAttempts(gcMaxAttempts),
		worker.WithBaseRetryDelay(gcRetryDelay),
	}
	// A handler that crashes, rather than returning an error, is caught by the
	// claim ceiling instead of the attempt count, as for mappers and reducers.
	if c.cfg.MaxClaims > 0 {
		opts = append(opts, worker.WithMaxClaims(c.cfg.MaxClaims))
	}
	return opts
}

// unavailableRetryDelay spaces out retries while the backend is unreachable,
// so an outage is waited out rather than hammered.
const unavailableRetryDelay = time.Second

// retryIfUnavailable turns an error caused by a temporarily unavailable backend
// into a retry of the task. Any other error is returned as it is: one that is
// not known to be transient stops the worker, for its orchestrator to restart.
func retryIfUnavailable(err error) error {
	if err == nil || !entroq.IsUnavailable(err) {
		return err
	}
	return worker.RetryErrorf("%v", err).After(unavailableRetryDelay)
}

// stateDoc reads the run's state doc. It returns a nil doc, not an error, when
// the run does not exist.
func (c *Controller) stateDoc(ctx context.Context) (*entroq.Doc, runState, error) {
	docs, err := c.client.Docs(ctx, &entroq.DocQuery{Namespace: c.DocNS(), KeyExact: stateDocKey})
	if err != nil {
		return nil, runState{}, fmt.Errorf("read run state: %w", err)
	}
	if len(docs) == 0 {
		return nil, runState{}, nil
	}
	st, err := entroq.GetContent[runState](docs[0])
	if err != nil {
		return nil, runState{}, fmt.Errorf("parse run state: %w", err)
	}
	return docs[0], st, nil
}

// readState reads the run's state without claiming it, for a handler that only
// needs to know whether the run has already ended. A run without a state doc
// cannot be worked on, so its task is quarantined.
func (c *Controller) readState(ctx context.Context) (runState, error) {
	doc, st, err := c.stateDoc(ctx)
	if err != nil {
		return st, err
	}
	if doc == nil {
		return st, worker.MoveErrorf("run %q has no state doc", c.prefix)
	}
	return st, nil
}

// stateDecision decides what one controller task commits, given the run state
// it holds claimed. It returns the task's own modifications and, if the run's
// state should change, the next state; nil leaves the state as it is.
type stateDecision[T any] func(ctx context.Context, task *entroq.Task, value T, st runState) ([]entroq.ModifyArg, *runState, error)

// stateWorker builds a controller worker that claims the state doc before it
// decides anything, and commits its decision together with the state doc's
// change and release. Holding the claim is what serializes the decisions that
// change the run as a whole.
func stateWorker[T any](c *Controller, decide stateDecision[T], opts ...worker.Option[T]) *worker.Worker[T] {
	always := func(context.Context) (bool, error) { return true, nil }
	return barrierWorker(c, always, decide, opts...)
}

// barrierWorker builds a controller worker whose task asks whether a barrier
// is satisfied. reached is a cheap check made without any claim; when it
// reports false the task is simply retired, which is the common case, and no
// run state is touched. Only when it reports true is the state doc claimed and
// decide called, and decide must then check again under the claim: reached
// saw the docs before the claim, and another check may have acted since.
func barrierWorker[T any](c *Controller, reached func(context.Context) (bool, error), decide stateDecision[T], opts ...worker.Option[T]) *worker.Worker[T] {
	opts = append(opts,
		worker.WithTakeDocs(func(ctx context.Context, _ *entroq.Task, _ T) ([]*entroq.DocClaim, error) {
			ok, err := reached(ctx)
			if err != nil && !entroq.IsUnavailable(err) {
				return nil, err
			}
			// A check that could not tell takes the slow path rather than
			// retiring, which would lose the signal: decide reads again under
			// the claim, where a failed read can be retried.
			if err == nil && !ok {
				return nil, nil
			}
			return []*entroq.DocClaim{entroq.ClaimKey(c.DocNS(), stateDocKey)}, nil
		}),
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, value T, docs []*entroq.Doc) (*worker.Result, error) {
			if len(docs) == 0 {
				// Not reached: nothing to decide.
				return worker.Modify(task.Delete()), nil
			}
			if len(docs) != 1 {
				return nil, worker.MoveErrorf("run %q has %d state docs, want 1", c.prefix, len(docs))
			}
			stateDoc := docs[0]
			st, err := entroq.GetContent[runState](stateDoc)
			if err != nil {
				return nil, worker.MoveErrorf("parse run state: %v", err)
			}
			mods, next, err := decide(ctx, task, value, st)
			if err != nil {
				return nil, retryIfUnavailable(err)
			}
			// A change releases the claim. One with no content releases it
			// without altering the state.
			release := stateDoc.Change()
			if next != nil {
				release = stateDoc.Change(entroq.WithContent(*next))
			}
			return worker.Modify(append(mods, release)...), nil
		}),
	)
	return worker.New[T](c.client, opts...)
}

// collecting returns the insert of a gc task for runs, or nothing for no runs.
func (c *Controller) collecting(runs []intermediateRun) []entroq.ModifyArg {
	if len(runs) == 0 {
		return nil
	}
	return []entroq.ModifyArg{entroq.InsertingInto(c.GCQ(), entroq.WithValue(gcTask{Runs: runs}))}
}

// checking returns the insert of a check task into queue. It goes in the same
// commit as the acceptance it follows, so it is durable and is handled only
// after that acceptance is visible: whichever acceptance is truly the last of
// its phase commits a check that finds the phase complete.
func checking(queue string) entroq.ModifyArg {
	return entroq.InsertingInto(queue, entroq.WithValue(barrierCheck{}))
}

// ending returns what every commit that ends a run carries, done or failed: a
// sweep of the run's store and sink. Intermediate data is useless once a run
// has ended either way, and is potentially large; what went wrong is the
// business of logs and the quarantine queue, not of data left behind.
func (c *Controller) ending(st runState) []entroq.ModifyArg {
	gc := gcTask{}
	if st.Store != (Descriptor{}) {
		gc.Stores = []Descriptor{st.Store}
	}
	if st.Sink != (Descriptor{}) {
		gc.Sinks = []Descriptor{st.Sink}
	}
	if len(gc.Stores) == 0 && len(gc.Sinks) == 0 {
		return nil
	}
	return []entroq.ModifyArg{entroq.InsertingInto(c.GCQ(), entroq.WithValue(gc))}
}

// failing ends the run with reason and consumes task by moving it to the
// quarantine queue, where reason stays with it for inspection. It is the
// disposition for a dependency the run cannot do without, such as a store or
// sink this deployment cannot reach: nothing can succeed until the submitter
// fixes the deployment and submits again.
func (c *Controller) failing(task *entroq.Task, st runState, reason string) ([]entroq.ModifyArg, *runState) {
	mods := []entroq.ModifyArg{task.Change(entroq.QueueTo(c.QuarantineQ()), entroq.AppendingErr(reason))}
	if st.terminal() {
		return mods, nil
	}
	next := st.failed(reason)
	return append(mods, c.ending(next)...), &next
}

// finishing tells the run's sink that its output is complete and returns the
// run's done state. It is called under the state claim, before the commit
// that marks the run done, so a run is never done before its output is ready
// to read. A crash between the two repeats it, which Sink.Finish allows.
func (c *Controller) finishing(ctx context.Context, task *entroq.Task, st runState, empty []int) ([]entroq.ModifyArg, *runState, error) {
	sink, err := c.sinks.resolve(st.Sink)
	if err != nil {
		mods, failed := c.failing(task, st, err.Error())
		return mods, failed, nil
	}
	if err := sink.Finish(ctx, c.prefix, st.ReduceParts, empty); err != nil {
		if _, ok := worker.AsMove(err); ok {
			mods, failed := c.failing(task, st, fmt.Sprintf("finish output: %v", err))
			return mods, failed, nil
		}
		return nil, nil, worker.RetryErrorf("finish output: %v", err)
	}
	next := st
	next.Phase = PhaseDone
	return append([]entroq.ModifyArg{task.Delete()}, c.ending(next)...), &next, nil
}

// mapDoneWorker accepts or rejects mapper finish tasks.
//
// It claims the finish's split doc, and nothing else, so acceptances of
// different splits never wait on one another. The first finish for a split
// wins: in one Modify the split doc is deleted, its runs are published as
// pointer docs, and a map check is queued. Any later finish for the same split
// finds the split gone and has its runs collected instead.
func (c *Controller) mapDoneWorker() *worker.Worker[mapDone] {
	return worker.New[mapDone](c.client,
		worker.WithTakeDocs(func(_ context.Context, _ *entroq.Task, md mapDone) ([]*entroq.DocClaim, error) {
			return []*entroq.DocClaim{entroq.ClaimKey(c.DocNS(), md.SplitKey)}, nil
		}),
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, md mapDone, docs []*entroq.Doc) (*worker.Result, error) {
			st, err := c.readState(ctx)
			if err != nil {
				return nil, retryIfUnavailable(err)
			}
			discard := append([]entroq.ModifyArg{task.Delete()}, c.collecting(md.Runs)...)
			if st.terminal() || len(docs) == 0 || docs[0].ID != md.Split.ID {
				// The run has ended, or another finish for this split was
				// accepted first.
				return worker.Modify(discard...), nil
			}
			mods := []entroq.ModifyArg{task.Delete(), docs[0].Delete(), checking(c.MapCheckQ())}
			for i, run := range md.Runs {
				if run.Partition < 0 || run.Partition >= st.ReduceParts {
					return nil, worker.MoveErrorf("split %q produced a run for partition %d of %d", md.SplitKey, run.Partition, st.ReduceParts)
				}
				mods = append(mods, entroq.PuttingDocInto(c.DocNS(),
					entroq.WithKeys(mapOutDocKey(run.Partition), mapOutSecondaryKey(md.SplitKey, i)),
					entroq.WithContent(run),
				))
			}
			return worker.Modify(mods...), nil
		}),
	)
}

// mapCheckWorker ends the map phase once no split docs remain. It fans out one
// reduce task per partition that received data, read from the published
// pointers' keys. A run in which no mapper emitted anything has nothing to
// reduce, so its output is finished, empty, and the run is done.
func (c *Controller) mapCheckWorker() *worker.Worker[barrierCheck] {
	reached := func(ctx context.Context) (bool, error) {
		left, err := c.anyDocs(ctx, splitPrefix)
		return !left, err
	}
	return barrierWorker(c, reached, func(ctx context.Context, task *entroq.Task, _ barrierCheck, st runState) ([]entroq.ModifyArg, *runState, error) {
		if st.Phase != PhaseMap {
			return []entroq.ModifyArg{task.Delete()}, nil, nil
		}
		if ok, err := reached(ctx); err != nil || !ok {
			return []entroq.ModifyArg{task.Delete()}, nil, err
		}
		parts, err := c.keyPartitions(ctx, mapOutPrefix)
		if err != nil {
			return nil, nil, err
		}
		if len(parts) == 0 {
			return c.finishing(ctx, task, st, allPartitions(st.ReduceParts))
		}
		next := st
		next.Phase = PhaseReduce
		mods := []entroq.ModifyArg{task.Delete()}
		for _, p := range parts {
			mods = append(mods, entroq.InsertingInto(c.ReduceQ(), entroq.WithValue(reduceTask{Partition: p, Sink: st.Sink})))
		}
		return mods, &next, nil
	})
}

// reduceDoneWorker accepts or rejects reducer finish tasks.
//
// It claims the partition's pointer docs, and nothing else, so acceptances of
// different partitions never wait on one another. The first finish for a
// partition wins: its staged output is committed to the sink, and then, in one
// Modify, the pointers it consumed are deleted, the partition's result marker
// is written, its runs are handed to the collector, and a reduce check is
// queued. A later finish for the same partition finds the pointers gone, and
// its runs are collected without recording anything.
//
// A losing finish's staged output is not discarded here, because whether it
// lost is a judgment about EntroQ state, and its output's fate was settled by
// the sink, outside EntroQ. A crash between a commit to the sink and the
// Modify that records it can leave a finish that looks like a loser and whose
// output is in fact the committed one. The sink alone knows, so staged output
// is collected only by sweeping the sink once the run has ended.
func (c *Controller) reduceDoneWorker() *worker.Worker[reduceDone] {
	return worker.New[reduceDone](c.client,
		worker.WithTakeDocs(func(_ context.Context, _ *entroq.Task, rd reduceDone) ([]*entroq.DocClaim, error) {
			return []*entroq.DocClaim{entroq.ClaimKey(c.DocNS(), mapOutDocKey(rd.Partition))}, nil
		}),
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, rd reduceDone, docs []*entroq.Doc) (*worker.Result, error) {
			st, err := c.readState(ctx)
			if err != nil {
				return nil, retryIfUnavailable(err)
			}
			discard := append([]entroq.ModifyArg{task.Delete()}, c.collecting(rd.Runs)...)
			if st.terminal() || len(docs) == 0 {
				// The run has ended, or another finish for this partition was
				// accepted first.
				return worker.Modify(discard...), nil
			}
			// Every map finished before any reduce task existed, so the pointer
			// set is fixed. A reducer that consumed a different set did not see
			// the whole partition, and its output would be silently incomplete.
			if !samePointers(docs, rd.Pointers) {
				return nil, worker.MoveErrorf("reducer for partition %d consumed %d map outputs, but %d are published", rd.Partition, len(rd.Pointers), len(docs))
			}

			// The sink is a grant the run cannot do without: quarantine the
			// task, and the error worker fails the run with this reason.
			sink, err := c.sinks.resolve(rd.Sink)
			if err != nil {
				return nil, worker.MoveErrorf("partition %d: %v", rd.Partition, err)
			}
			// Commit the staged output before recording it. A crash in between
			// leaves this finish task to be handled again, and committing it
			// again is a no-op the Sink contract requires.
			if err := sink.Commit(ctx, c.prefix, rd.Partition, rd.Staged); err != nil {
				return nil, sinkFailure(err, "commit output for partition %d", rd.Partition)
			}

			mods := []entroq.ModifyArg{task.Delete(), checking(c.ReduceCheckQ())}
			for _, d := range docs {
				mods = append(mods, d.Delete())
			}
			// The result marker records that the partition is committed; the
			// output itself lives in the sink.
			mods = append(mods, entroq.PuttingDocInto(c.DocNS(),
				entroq.WithIDKeys(resultDocID(rd.Partition), resultDocKey(rd.Partition), ""),
			))
			return worker.Modify(append(mods, c.collecting(rd.Runs)...)...), nil
		}),
	)
}

// reduceCheckWorker ends the reduce phase once no map-output pointers remain:
// every partition that received data has had its output committed. It
// finishes the sink, telling it which partitions received none, and marks the
// run done.
func (c *Controller) reduceCheckWorker() *worker.Worker[barrierCheck] {
	reached := func(ctx context.Context) (bool, error) {
		left, err := c.anyDocs(ctx, mapOutPrefix)
		return !left, err
	}
	return barrierWorker(c, reached, func(ctx context.Context, task *entroq.Task, _ barrierCheck, st runState) ([]entroq.ModifyArg, *runState, error) {
		if st.Phase != PhaseReduce {
			return []entroq.ModifyArg{task.Delete()}, nil, nil
		}
		if ok, err := reached(ctx); err != nil || !ok {
			return []entroq.ModifyArg{task.Delete()}, nil, err
		}
		done, err := c.keyPartitions(ctx, resultPrefix)
		if err != nil {
			return nil, nil, err
		}
		var empty []int
		for _, p := range allPartitions(st.ReduceParts) {
			if _, ok := slices.BinarySearch(done, p); !ok {
				empty = append(empty, p)
			}
		}
		return c.finishing(ctx, task, st, empty)
	})
}

// allPartitions lists partitions 0 through n-1.
func allPartitions(n int) []int {
	parts := make([]int, n)
	for i := range parts {
		parts[i] = i
	}
	return parts
}

// samePointers reports whether the published pointer docs are exactly the ones
// a reducer consumed. Claiming them changes their versions, and pointer docs
// are never rewritten, so their ids alone identify them.
func samePointers(published []*entroq.Doc, consumed []entroq.DocID) bool {
	if len(published) != len(consumed) {
		return false
	}
	ids := make(map[string]bool, len(published))
	for _, d := range published {
		ids[d.ID] = true
	}
	for _, p := range consumed {
		if !ids[p.ID] {
			return false
		}
	}
	return true
}

// anyDocs reports whether any doc in the run's namespace has a key under
// prefix. It is a best-effort read: the answer may be stale by the time the
// caller acts, so a caller that changes run state on it checks again under the
// state claim.
func (c *Controller) anyDocs(ctx context.Context, prefix string) (bool, error) {
	docs, err := c.client.Docs(ctx, &entroq.DocQuery{
		Namespace:  c.DocNS(),
		KeyStart:   prefix,
		KeyEnd:     prefixEnd(prefix),
		OmitValues: true,
		Limit:      1,
	})
	if err != nil {
		return false, fmt.Errorf("check %q docs: %w", prefix, err)
	}
	return len(docs) > 0, nil
}

// keyPartitions lists, in order and without repeats, the partitions named by
// the keys of docs under prefix, whose keys end in a partition number.
func (c *Controller) keyPartitions(ctx context.Context, prefix string) ([]int, error) {
	docs, err := c.client.Docs(ctx, &entroq.DocQuery{
		Namespace:  c.DocNS(),
		KeyStart:   prefix,
		KeyEnd:     prefixEnd(prefix),
		OmitValues: true,
	})
	if err != nil {
		return nil, fmt.Errorf("list %q docs: %w", prefix, err)
	}
	var parts []int
	for _, d := range docs {
		p, err := strconv.Atoi(strings.TrimPrefix(d.Key, prefix))
		if err != nil {
			return nil, fmt.Errorf("doc key %q names no partition: %w", d.Key, err)
		}
		if i, found := slices.BinarySearch(parts, p); !found {
			parts = slices.Insert(parts, i, p)
		}
	}
	return parts, nil
}

// gcWorker deletes intermediate runs and sweeps ended runs. Deletes are
// idempotent, so a task that is retried after partial progress simply deletes
// what is left.
//
// It never touches run state. A run whose collection fails is still a correct
// run; the failure is an operator's storage problem, not a reason to fail it,
// so an exhausted collection task is quarantined without failing anything.
func (c *Controller) gcWorker() *worker.Worker[gcTask] {
	return worker.New[gcTask](c.client,
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, gc gcTask, _ []*entroq.Doc) (*worker.Result, error) {
			for i, run := range gc.Runs {
				store, err := c.stores.resolve(run.Store)
				if err != nil {
					return nil, worker.MoveErrorf("collect run %d: %v", i, err)
				}
				if err := store.delete(ctx, run.Ref); err != nil {
					return nil, worker.RetryErrorf("collect run %d: %v", i, err)
				}
			}
			// Sweeping is best effort: it collects only what nothing
			// references, so a store that cannot, or a sweep that fails, leaves
			// garbage for an operator rather than a wrong result.
			for _, d := range gc.Stores {
				store, err := c.stores.resolve(d)
				if err == nil {
					err = store.sweep(ctx, c.prefix)
				}
				logSweepFailure("intermediate store", d, c.prefix, err)
			}
			for _, d := range gc.Sinks {
				sink, err := c.sinks.resolve(d)
				if err == nil {
					err = sink.Sweep(ctx, c.prefix)
				}
				logSweepFailure("sink", d, c.prefix, err)
			}
			return worker.Modify(task.Delete()), nil
		}),
	)
}

// logSweepFailure reports a failed sweep. Sweeping is best effort, so the
// failure is logged rather than retried: what it would have collected is
// garbage, not a wrong result.
func logSweepFailure(what string, d Descriptor, run string, err error) {
	if err == nil {
		return
	}
	log.Printf("eqmr: sweep %s %s for %q: %v", what, d, run, err)
}

// checkGrants fails an existing, unfinished run that names a store or sink
// this process cannot reach. A run with no state doc yet is left alone: a
// controller may start before Setup.
func (c *Controller) checkGrants(ctx context.Context) error {
	_, st, err := c.stateDoc(ctx)
	if err != nil {
		return fmt.Errorf("eqmr controller: %w", err)
	}
	if st.Phase == "" || st.terminal() {
		return nil
	}
	var missing []error
	if _, err := c.stores.resolve(st.Store); err != nil {
		missing = append(missing, err)
	}
	if _, err := c.sinks.resolve(st.Sink); err != nil {
		missing = append(missing, err)
	}
	if len(missing) == 0 {
		return nil
	}
	reason := fmt.Sprintf("controller cannot run: %v", errors.Join(missing...))
	if err := c.failNow(ctx, reason); err != nil {
		return fmt.Errorf("eqmr controller: %s; failing the run: %w", reason, err)
	}
	return fmt.Errorf("eqmr controller: %s", reason)
}

// failNow fails the run from outside any worker, unless it has already ended.
func (c *Controller) failNow(ctx context.Context, reason string) error {
	return c.updateState(ctx, func(st runState) (*runState, []entroq.ModifyArg) {
		if st.terminal() {
			return nil, nil
		}
		next := st.failed(reason)
		return &next, c.ending(next)
	})
}

// updateState changes the run state from outside any worker. It claims the
// state doc as a claimant of its own, waiting out any handler that holds it,
// and commits what change returns with the claim's release: the next state,
// or nil to leave it, and anything else that must land in the same commit.
func (c *Controller) updateState(ctx context.Context, change func(runState) (*runState, []entroq.ModifyArg)) error {
	claimant := entroq.GenHex16()
	for {
		docs, err := c.client.ClaimDocs(ctx, &entroq.DocClaim{
			Namespace: c.DocNS(),
			Key:       stateDocKey,
			Claimant:  claimant,
			Duration:  c.cfg.Lease,
		})
		if _, ok := entroq.AsDependency(err); ok {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(maxWaitInterval):
			}
			continue
		}
		if err != nil {
			return err
		}
		if len(docs) != 1 {
			return fmt.Errorf("run %q has %d state docs, want 1", c.prefix, len(docs))
		}
		st, err := entroq.GetContent[runState](docs[0])
		if err != nil {
			return fmt.Errorf("parse run state: %w", err)
		}
		next, mods := change(st)
		release := docs[0].Change()
		if next != nil {
			release = docs[0].Change(entroq.WithContent(*next))
		}
		_, err = c.client.Modify(ctx, append(mods, release, entroq.ModifyAs(claimant))...)
		return err
	}
}

// errWorker judges every task that a run's workers quarantined.
//
// A quarantined map or reduce task may be a loser rather than a failure: a
// duplicate whose unit another worker already completed can still exhaust its
// claims. Those are retired. Anything else fails the run, with the task's own
// error as the reason, and moves to QuarantineQ so it stays inspectable. A
// collection failure never fails the run, because the run's output is
// unaffected; it is kept for an operator.
func (c *Controller) errWorker() *worker.Worker[json.RawMessage] {
	return stateWorker(c, func(ctx context.Context, task *entroq.Task, raw json.RawMessage, st runState) ([]entroq.ModifyArg, *runState, error) {
		origin := strings.TrimSuffix(task.Queue, errQSuffix)
		loser, err := c.isLoser(ctx, origin, raw)
		if err != nil {
			return nil, nil, err
		}
		if loser {
			return []entroq.ModifyArg{task.Delete()}, nil, nil
		}

		mods := []entroq.ModifyArg{task.Change(entroq.QueueTo(c.QuarantineQ()))}
		if origin == c.GCQ() || st.terminal() {
			return mods, nil, nil
		}
		next := st.failed(fmt.Sprintf("task quarantined from %s: %s", origin, task.Err))
		return append(mods, c.ending(next)...), &next, nil
	}, worker.WithErrQMap[json.RawMessage](func(string) string { return c.QuarantineQ() }))
}

// isLoser reports whether a quarantined task from origin is a duplicate whose
// unit was already completed by another worker.
func (c *Controller) isLoser(ctx context.Context, origin string, raw json.RawMessage) (bool, error) {
	// A value that does not parse cannot be shown to be a loser, so it is
	// judged a real failure: its parse error is not discarded, it is the
	// verdict.
	switch origin {
	case c.MapQ():
		var ref docRef
		if err := json.Unmarshal(raw, &ref); err != nil {
			return false, nil
		}
		found, err := c.client.Docs(ctx, ref.asQuery())
		if err != nil {
			return false, fmt.Errorf("check split %q: %w", ref.Key, err)
		}
		return len(found) == 0, nil

	case c.ReduceQ():
		var rt reduceTask
		if err := json.Unmarshal(raw, &rt); err != nil {
			return false, nil
		}
		found, err := c.client.Docs(ctx, &entroq.DocQuery{
			Namespace:  c.DocNS(),
			KeyExact:   resultDocKey(rt.Partition),
			OmitValues: true,
		})
		if err != nil {
			return false, fmt.Errorf("check result %d: %w", rt.Partition, err)
		}
		return len(found) > 0, nil

	default:
		return false, nil
	}
}
