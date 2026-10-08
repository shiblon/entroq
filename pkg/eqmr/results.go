package eqmr

import (
	"context"
	"fmt"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
	"golang.org/x/sync/errgroup"
)

// Results returns every output pair from a completed run, sorted by key.
//
// It reads the run's document sink, the default when no WithSink is given.
// Output sent to any other sink is read with that sink's own reader: the run
// does not know its format.
//
// Each partition is already sorted by key, so this is a k-way merge rather than
// a sort. It loads the whole output into memory; a run whose output does not
// fit should read partitions individually with ResultsForPartition instead.
func (c *Controller) Results(ctx context.Context) ([]*KV, error) {
	sink, err := c.completedDocumentSink(ctx)
	if err != nil {
		return nil, fmt.Errorf("eqmr results: %w", err)
	}
	m, err := sink.manifest(ctx)
	if err != nil {
		return nil, fmt.Errorf("eqmr results: %w", err)
	}
	runs := make([][]*KV, 0, m.Partitions)
	for p := range m.Partitions {
		out, err := sink.readPartition(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("eqmr results: %w", err)
		}
		if len(out) > 0 {
			runs = append(runs, out)
		}
	}
	return mergeKVRuns(runs), nil
}

// ResultsForPartition returns the output pairs for one reduce partition, sorted
// by key. An empty slice means no mapper emitted a key belonging to it. Like
// Results, it reads the run's document sink.
//
// Prefer Results unless you are deliberately reading partition by partition;
// this is the escape hatch for output too large to hold at once.
func (c *Controller) ResultsForPartition(ctx context.Context, p int) ([]*KV, error) {
	if p < 0 {
		return nil, fmt.Errorf("eqmr results: negative partition %d", p)
	}
	sink, err := c.completedDocumentSink(ctx)
	if err != nil {
		return nil, fmt.Errorf("eqmr results partition %d: %w", p, err)
	}
	out, err := sink.readPartition(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("eqmr results partition %d: %w", p, err)
	}
	return out, nil
}

// completedDocumentSink returns the run's document sink once the run is done.
func (c *Controller) completedDocumentSink(ctx context.Context) (*documentSink, error) {
	if err := c.requireCompletedRun(ctx); err != nil {
		return nil, err
	}
	_, st, err := c.stateDoc(ctx)
	if err != nil {
		return nil, err
	}
	sink, err := c.sinks.resolve(st.Sink)
	if err != nil {
		return nil, err
	}
	docs, ok := sink.(*documentSink)
	if !ok {
		return nil, fmt.Errorf("output went to sink %s; read it with that sink", st.Sink)
	}
	return docs, nil
}

func (c *Controller) requireCompletedRun(ctx context.Context) error {
	phase, reason, err := c.Status(ctx)
	if err != nil {
		return err
	}
	switch phase {
	case PhaseDone:
		return nil
	case PhaseFailed:
		return fmt.Errorf("run failed: %s", reason)
	default:
		return fmt.Errorf("run is in %q phase, not %q", phase, PhaseDone)
	}
}

// mergeKVRuns merges key-sorted runs into one key-sorted slice. Partitions are
// disjoint by construction, so no key appears in more than one run and there is
// nothing to combine on a tie.
func mergeKVRuns(runs [][]*KV) []*KV {
	total := 0
	for _, r := range runs {
		total += len(r)
	}
	out := make([]*KV, 0, total)

	cursors := make([]int, len(runs))
	for range total {
		best := -1
		for i, run := range runs {
			if cursors[i] >= len(run) {
				continue
			}
			if best < 0 || run[cursors[i]].Key < runs[best][cursors[best]].Key {
				best = i
			}
		}
		if best < 0 {
			break
		}
		out = append(out, runs[best][cursors[best]])
		cursors[best]++
	}
	return out
}

// RunOptions configures the single-process Run helper.
type RunOptions struct {
	// Mappers is the number of mapper workers to run in this process.
	Mappers int
	// Reducers is the number of reducer workers to run in this process.
	Reducers int
	// MaxAttempts is how many times a task returning a RetryError may be
	// retried before it is quarantined and fails the run. Zero means the
	// worker default.
	MaxAttempts int32

	// Combiner, if set, is applied by every mapper.
	Combiner Combiner
	// IntermediateRunBytes bounds each mapper's in-memory sorting buffer.
	// Non-positive values use DefaultIntermediateRunBytes.
	IntermediateRunBytes int
}

// Run performs a whole MapReduce in this process: Setup, then mapper, reducer,
// and controller workers, then Wait.
//
// It exists for tests, benchmarks, and small single-binary jobs. The deployed
// shape is the opposite of this: Setup once, then run RunMapper, RunReducer,
// and RunController as separate scalable deployments against the same Prefix,
// each of which needs nothing from the others but the queue names.
//
// Unlike a naive supervisor, a worker that exits does not cancel the run.
// Workers are restarted, because a mapper is expected to die on a bad input and
// the correct response is to let the task be reclaimed and retried until it
// exhausts its attempts, at which point the controller fails the run.
func (c *Controller) Run(ctx context.Context, input []*KV, mapFn Mapper, reduceFn Reducer, opts RunOptions) error {
	if opts.Mappers <= 0 || opts.Reducers <= 0 {
		return fmt.Errorf("eqmr run: Mappers and Reducers must both be positive, got %d and %d", opts.Mappers, opts.Reducers)
	}
	if err := c.Setup(ctx, input); err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, gctx := errgroup.WithContext(runCtx)

	// Work queues get the claim ceiling from Config; controller queues must not
	// have one, which workerRunOptions and controlRunOptions encode.
	workOpts := func(q string) []worker.RunOption {
		ro := c.workerRunOptions(q)
		if opts.MaxAttempts > 0 {
			ro = append(ro, worker.WithMaxAttempts(opts.MaxAttempts))
		}
		return ro
	}
	var mapperOpts []MapperOption
	if opts.Combiner != nil {
		mapperOpts = append(mapperOpts, WithCombiner(opts.Combiner))
	}
	if opts.IntermediateRunBytes > 0 {
		mapperOpts = append(mapperOpts, WithIntermediateRunBytes(opts.IntermediateRunBytes))
	}

	for range opts.Mappers {
		g.Go(func() error {
			return restarting(gctx, func(ctx context.Context) error {
				return c.MapperWorker(mapFn, mapperOpts...).Run(ctx, workOpts(c.MapQ())...)
			})
		})
	}
	for range opts.Reducers {
		g.Go(func() error {
			return restarting(gctx, func(ctx context.Context) error {
				return c.ReducerWorker(reduceFn).Run(ctx, workOpts(c.ReduceQ())...)
			})
		})
	}
	for _, role := range c.controllerRoles() {
		g.Go(func() error { return restarting(gctx, role) })
	}

	waitErr := c.Wait(runCtx)
	if waitErr == nil {
		// The controller workers run in this process too, so they must stay up
		// until they have collected what the run left behind: the last reduce
		// finish hands its runs to the collector in the same commit that ends
		// the run.
		waitErr = c.awaitCollection(runCtx)
	}
	cancel()
	if err := g.Wait(); err != nil && waitErr == nil {
		waitErr = err
	}
	return waitErr
}

// awaitCollection waits until no finish or collection work is outstanding, so
// every intermediate run the controller will ever delete has been deleted.
func (c *Controller) awaitCollection(ctx context.Context) error {
	ticker := time.NewTicker(maxWaitInterval)
	defer ticker.Stop()
	for {
		stats, err := c.client.QueueStats(ctx, entroq.MatchExact(c.MapDoneQ(), c.MapCheckQ(), c.ReduceDoneQ(), c.ReduceCheckQ(), c.GCQ()))
		if err != nil {
			return fmt.Errorf("eqmr run: await collection: %w", err)
		}
		pending := 0
		for _, s := range stats {
			pending += s.Size
		}
		if pending == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("eqmr run: await collection: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// restartDelay paces worker restarts so a persistently failing handler cannot
// spin hot. A deployed run gets this from its orchestrator's backoff instead.
const restartDelay = 100 * time.Millisecond

// restarting keeps a worker alive across handler-fatal exits, standing in for
// the process orchestrator that would do this in a deployed run.
func restarting(ctx context.Context, run func(context.Context) error) error {
	for {
		err := run(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return nil
		}
		// The controller decides when repeated failures end the run, via the
		// quarantine queue. Restarting here mirrors what Kubernetes or systemd
		// would do, so that policy stays in one place.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(restartDelay):
		}
	}
}
