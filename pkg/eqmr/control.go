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

// mapDone is a mapper's finish task. Its runs are written but unpublished until
// the controller accepts it.
type mapDone struct {
	// SplitKey is the split doc's key, for claiming; Split identifies the doc
	// the mapper read.
	SplitKey string            `json:"split_key"`
	Split    entroq.DocID      `json:"split"`
	Runs     []intermediateRun `json:"runs"`
}

// reduceDone is a reducer's finish task: a reference to its staged output, and
// the pointers and runs it consumed.
type reduceDone struct {
	Partition int               `json:"partition"`
	Pointers  []entroq.DocID    `json:"pointers"`
	Runs      []intermediateRun `json:"runs"`
	Sink      Descriptor        `json:"sink"`
	Staged    json.RawMessage   `json:"staged"`
}

// barrierCheck is the value of a check task, which asks whether a phase is
// complete.
type barrierCheck struct{}

// gcTask asks the collector to delete Runs, which were retired inside one
// EntroQ commit, and to sweep an ended run's Stores and Sinks of everything
// nothing committed.
type gcTask struct {
	Runs   []intermediateRun `json:"runs,omitempty"`
	Stores []Descriptor      `json:"stores,omitempty"`
	Sinks  []Descriptor      `json:"sinks,omitempty"`
}

// RunController runs this run's controller workers until ctx ends or one exits.
// Run any number of replicas.
//
// It first fails an unfinished run that names a store or sink this process
// cannot reach, and returns an error.
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

// controllerRoles binds each controller worker to its queues, for RunController
// and Run.
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

// controlRunOptions configures a controller worker. Each run has its own
// claimant, the worker default, so doc claims exclude the other controller
// workers on this client. A doc found held is about to be released, so its
// task is retried at once and has no claim ceiling.
func (c *Controller) controlRunOptions(queues ...string) []worker.RunOption {
	return []worker.RunOption{
		worker.Watching(queues...),
		worker.WithLease(c.cfg.Lease),
		worker.WithBaseRetryDelay(0),
	}
}

// Collection retries. An exhausted collection task is quarantined, which does
// not fail the run.
const (
	gcMaxAttempts = 10
	gcRetryDelay  = time.Second
)

// gcRunOptions configures the collector.
func (c *Controller) gcRunOptions() []worker.RunOption {
	opts := []worker.RunOption{
		worker.Watching(c.GCQ()),
		worker.WithLease(c.cfg.Lease),
		worker.WithMaxAttempts(gcMaxAttempts),
		worker.WithBaseRetryDelay(gcRetryDelay),
	}
	// The claim ceiling catches a handler that crashes.
	if c.cfg.MaxClaims > 0 {
		opts = append(opts, worker.WithMaxClaims(c.cfg.MaxClaims))
	}
	return opts
}

// unavailableRetryDelay spaces out retries while the backend is unreachable.
const unavailableRetryDelay = time.Second

// retryIfUnavailable turns an error from a temporarily unavailable backend into
// a retry, and returns any other error unchanged.
func retryIfUnavailable(err error) error {
	if err == nil || !entroq.IsUnavailable(err) {
		return err
	}
	return worker.RetryErrorf("%v", err).After(unavailableRetryDelay)
}

// stateDoc reads the run's state doc, which is nil if the run does not exist.
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

// readState reads the run state without a claim. A run with no state doc
// quarantines the task.
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

// stateDecision decides a controller task's commit from the claimed run state:
// the task's own modifications, and the next state, or nil to keep it.
type stateDecision[T any] func(ctx context.Context, task *entroq.Task, value T, st runState) ([]entroq.ModifyArg, *runState, error)

// stateWorker builds a controller worker that claims the state doc, decides,
// and commits the decision with the state doc's change and release.
func stateWorker[T any](c *Controller, decide stateDecision[T], opts ...worker.Option[T]) *worker.Worker[T] {
	always := func(context.Context) (bool, error) { return true, nil }
	return barrierWorker(c, always, decide, opts...)
}

// barrierWorker builds a controller worker for check tasks. reached looks
// without a claim, and the task retires if it is false. Otherwise the state doc
// is claimed and decide is called; decide must check again under the claim.
func barrierWorker[T any](c *Controller, reached func(context.Context) (bool, error), decide stateDecision[T], opts ...worker.Option[T]) *worker.Worker[T] {
	opts = append(opts,
		worker.WithTakeDocs(func(ctx context.Context, _ *entroq.Task, _ T) ([]*entroq.DocClaim, error) {
			ok, err := reached(ctx)
			if err != nil && !entroq.IsUnavailable(err) {
				return nil, err
			}
			// A check that could not tell must not retire: it claims, and
			// decide's read can be retried.
			if err == nil && !ok {
				return nil, nil
			}
			return []*entroq.DocClaim{entroq.ClaimKey(c.DocNS(), stateDocKey)}, nil
		}),
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, value T, docs []*entroq.Doc) (*worker.Result, error) {
			if len(docs) == 0 {
				// Not reached.
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
			// A change releases the claim; one without content keeps the state.
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

// checking inserts a check task into queue. It belongs in the commit it
// follows, so the check is handled after that commit is visible, and the last
// commit of a phase queues a check that finds the phase complete.
func checking(queue string) entroq.ModifyArg {
	return entroq.InsertingInto(queue, entroq.WithValue(barrierCheck{}))
}

// ending is what every commit that ends a run carries: a sweep of its store
// and sink.
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

// failing fails the run with reason and moves task to the quarantine queue,
// for a dependency the run cannot do without.
func (c *Controller) failing(task *entroq.Task, st runState, reason string) ([]entroq.ModifyArg, *runState) {
	mods := []entroq.ModifyArg{task.Change(entroq.QueueTo(c.QuarantineQ()), entroq.AppendingErr(reason))}
	if st.terminal() {
		return mods, nil
	}
	next := st.failed(reason)
	return append(mods, c.ending(next)...), &next
}

// finishing calls the sink's Finish, under the state claim, and returns the
// commit that marks the run done, so a done run's output is ready to read.
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

// mapDoneWorker accepts the first finish for each split. Holding only the
// split doc, it deletes it, publishes the runs as pointer docs, and queues a
// map check, in one Modify. A later finish finds the split gone, and its runs
// are collected.
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
				// The run has ended, or the split was already accepted.
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

// mapCheckWorker ends the map phase once no split docs remain: it fans out a
// reduce task for each partition with pointer docs, or finishes the run if
// there are none.
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

// reduceDoneWorker accepts the first finish for each partition. Holding only
// the partition's pointer docs, it commits the staged output to the sink, then
// in one Modify deletes the pointers, writes the result marker, collects the
// consumed runs, and queues a reduce check. A later finish finds the pointers
// gone, and its runs are collected.
//
// A loser's staged output is left for the sink's sweep: a crash between the
// sink commit and the Modify can make the committed attempt look like a loser.
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
				// The run has ended, or the partition was already accepted.
				return worker.Modify(discard...), nil
			}
			// The pointer set is fixed once mapping ends; a reducer that read a
			// different set missed part of the partition.
			if !samePointers(docs, rd.Pointers) {
				return nil, worker.MoveErrorf("reducer for partition %d consumed %d map outputs, but %d are published", rd.Partition, len(rd.Pointers), len(docs))
			}

			// A sink this process cannot reach fails the run.
			sink, err := c.sinks.resolve(rd.Sink)
			if err != nil {
				return nil, worker.MoveErrorf("partition %d: %v", rd.Partition, err)
			}
			// Committing again after a crash is a no-op.
			if err := sink.Commit(ctx, c.prefix, rd.Partition, rd.Staged); err != nil {
				return nil, sinkFailure(err, "commit output for partition %d", rd.Partition)
			}

			mods := []entroq.ModifyArg{task.Delete(), checking(c.ReduceCheckQ())}
			for _, d := range docs {
				mods = append(mods, d.Delete())
			}
			// The marker records the partition as committed; output lives in
			// the sink.
			mods = append(mods, entroq.PuttingDocInto(c.DocNS(),
				entroq.WithIDKeys(resultDocID(rd.Partition), resultDocKey(rd.Partition), ""),
			))
			return worker.Modify(append(mods, c.collecting(rd.Runs)...)...), nil
		}),
	)
}

// reduceCheckWorker ends the reduce phase once no pointer docs remain: it
// finishes the sink, naming the partitions with no data, and marks the run
// done.
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

// samePointers reports whether published and consumed name the same pointer
// docs. Claiming changes their versions, so ids identify them.
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

// anyDocs reports whether any doc has a key under prefix. It takes no claim, so
// its answer can be stale.
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

// gcWorker deletes retired runs and sweeps ended runs. Deletes are idempotent,
// so a retry deletes what is left. Collection never touches run state.
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
			// Sweeps are best effort: what one misses is garbage.
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

// logSweepFailure logs a failed sweep, which is not retried.
func logSweepFailure(what string, d Descriptor, run string, err error) {
	if err == nil {
		return
	}
	log.Printf("eqmr: sweep %s %s for %q: %v", what, d, run, err)
}

// checkGrants fails an unfinished run that names a store or sink this process
// cannot reach. A run with no state doc yet is left alone.
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

// updateState changes the run state from outside a worker. It claims the state
// doc under its own claimant, waiting for any holder, and commits what change
// returns, the next state or nil and any other modifications, with the
// release.
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

// errWorker judges quarantined tasks. A duplicate whose unit is already
// complete is retired. Anything else moves to QuarantineQ and fails the run with
// the task's error, except a collection failure, which is only kept.
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

// isLoser reports whether a quarantined task from origin duplicates a unit that
// is already complete.
func (c *Controller) isLoser(ctx context.Context, origin string, raw json.RawMessage) (bool, error) {
	// A value that does not parse cannot be shown to be a loser, so it counts
	// as a failure.
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
