package eqmr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
	"golang.org/x/sync/errgroup"
)

// Results returns every output pair from a completed run, sorted by key.
//
// Output lives in one result doc per reduce partition, each already sorted by
// key, so this is a k-way merge rather than a sort. It loads the whole output
// into memory; a run whose output does not fit should read partitions
// individually with ResultsForPartition instead.
func (c *Controller) Results(ctx context.Context) ([]*KV, error) {
	runs := make([][]*KV, 0, c.cfg.ReduceShards)
	for p := range c.cfg.ReduceShards {
		part, err := c.ResultsForPartition(ctx, p)
		if err != nil {
			return nil, err
		}
		if len(part) > 0 {
			runs = append(runs, part)
		}
	}
	return mergeKVRuns(runs), nil
}

// ResultsForPartition returns the output pairs for one reduce partition, sorted
// by key. An empty slice means no mapper emitted a key belonging to it.
func (c *Controller) ResultsForPartition(ctx context.Context, p int) ([]*KV, error) {
	if p < 0 || p >= c.cfg.ReduceShards {
		return nil, fmt.Errorf("eqmr results: partition %d out of range [0,%d)", p, c.cfg.ReduceShards)
	}
	docs, err := c.client.Docs(ctx, &entroq.DocQuery{
		Namespace: c.DocNS(),
		KeyExact:  resultDocKey(p),
	})
	if err != nil {
		return nil, fmt.Errorf("eqmr results partition %d: %w", p, err)
	}
	if len(docs) == 0 {
		return nil, nil
	}
	var out []*KV
	if err := json.Unmarshal(docs[0].Content, &out); err != nil {
		return nil, fmt.Errorf("eqmr results partition %d: parse: %w", p, err)
	}
	return out, nil
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
			if best < 0 || bytes.Compare(run[cursors[i]].Key, runs[best][cursors[best]].Key) < 0 {
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

// Cleanup removes everything a run created: all of its docs and all of its
// tasks, including the control task and anything quarantined.
//
// It is safe to call on a partially completed run, but not on a running one: a
// claimed doc or task cannot be deleted, and Cleanup reports that as an error
// rather than partially tearing down a live run.
//
// Cleanup is the explicit path. A run whose output should simply expire can
// instead put its namespace under EntroQ's /gc= convention, but note that
// activation is baked into the namespace string at insert time and collects any
// unclaimed doc group once it fires, which is wrong for intermediate docs and
// right only for output a consumer has a bounded window to read.
func (c *Controller) Cleanup(ctx context.Context) error {
	const batch = 250

	for {
		docs, err := c.client.Docs(ctx, &entroq.DocQuery{
			Namespace:  c.DocNS(),
			OmitValues: true,
			Limit:      batch,
		})
		if err != nil {
			return fmt.Errorf("eqmr cleanup: list docs: %w", err)
		}
		if len(docs) == 0 {
			break
		}
		args := make([]entroq.ModifyArg, 0, len(docs))
		for _, d := range docs {
			args = append(args, d.Delete())
		}
		if _, err := c.client.Modify(ctx, args...); err != nil {
			return fmt.Errorf("eqmr cleanup: delete docs (a claimed doc means the run is still live): %w", err)
		}
	}

	for _, q := range []string{c.MapQ(), c.ReduceQ(), c.ControlQ(), c.ErrQ()} {
		for {
			tasks, err := c.client.Tasks(ctx, q)
			if err != nil {
				return fmt.Errorf("eqmr cleanup: list %q: %w", q, err)
			}
			if len(tasks) == 0 {
				break
			}
			args := make([]entroq.ModifyArg, 0, len(tasks))
			for _, t := range tasks {
				args = append(args, t.Delete())
			}
			if _, err := c.client.Modify(ctx, args...); err != nil {
				return fmt.Errorf("eqmr cleanup: delete tasks in %q (a claimed task means the run is still live): %w", q, err)
			}
		}
	}
	return nil
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

	// MaxClaims is how many times a task may be claimed at all before it is
	// quarantined without being handed to a handler. This is the bound that
	// actually catches poison input: a Mapper that dies on a bad record kills
	// its worker without marking the task, so the task is simply reclaimed
	// after its lease expires and kills the next worker too. MaxClaims turns
	// that loop into a run failure after a bounded number of tries, which is
	// what "a crash is recoverable up to a point" means in practice.
	//
	// Zero means unlimited, and a run with a genuinely poisonous input will
	// then retry it forever. Set it.
	MaxClaims int32
	// Combiner, if set, is applied by every mapper.
	Combiner Combiner
}

// Run performs a whole MapReduce in this process: Setup, then mapper, reducer,
// and control workers, then Wait.
//
// It exists for tests, benchmarks, and small single-binary jobs. The deployed
// shape is the opposite of this: Setup once, then run MapperWorker,
// ReducerWorker, and ControlWorker as separate scalable deployments against the
// same Prefix, each of which needs nothing from the others but the queue names.
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

	// Work queues get the retry and claim bounds; the control queue must not.
	// The control task is deliberately claimed once per tick for the life of
	// the run, so a claim bound would quarantine the controller after a few
	// seconds of perfectly healthy operation, leaving the run with nothing
	// driving it and nothing left to notice.
	workOpts := func(q string) []worker.RunOption {
		ro := []worker.RunOption{worker.Watching(q), worker.WithLease(c.cfg.Lease)}
		if opts.MaxAttempts > 0 {
			ro = append(ro, worker.WithMaxAttempts(opts.MaxAttempts))
		}
		if opts.MaxClaims > 0 {
			ro = append(ro, worker.WithMaxClaims(opts.MaxClaims))
		}
		return ro
	}
	controlOpts := []worker.RunOption{worker.Watching(c.ControlQ()), worker.WithLease(c.cfg.Lease)}

	var mapperOpts []MapperOption
	if opts.Combiner != nil {
		mapperOpts = append(mapperOpts, WithCombiner(opts.Combiner))
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
	g.Go(func() error {
		return restarting(gctx, func(ctx context.Context) error {
			return c.ControlWorker().Run(ctx, controlOpts...)
		})
	})

	waitErr := c.Wait(runCtx)
	cancel()
	if err := g.Wait(); err != nil && waitErr == nil {
		waitErr = err
	}
	return waitErr
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
