package eqmr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

// reduceTask is the value of a reduce task: one partition of the shuffle.
type reduceTask struct {
	Partition int `json:"partition"`
	// Sink is the run's sink, stamped by the controller from the run state so
	// a reducer process needs no configuration of the run's own.
	Sink Descriptor `json:"sink"`
}

// Setup writes the input splits and starts the run. It divides input into
// Config.MapShards contiguous splits, one split doc and one map task per
// non-empty split.
//
// Setup stages, then commits. The split docs are written first, in batches,
// under deterministic ids, and are inert: nothing refers to them, so no run
// exists yet. Then one Modify creates the run, its state doc and every map task
// together. Before that commit there is no run; after it there is a complete
// one, so a run is never seen half set up.
//
// Setup is therefore safe to retry with the same input. A retry reuses the
// splits already staged, and one whose commit already landed finds this run
// in place and succeeds. An attempt abandoned before its commit leaves only
// inert split docs, with no run to act on them. A Prefix that already holds a
// different run is an error; use a fresh Prefix per run.
//
// Empty input is an error: there would be nothing to map, so nothing would
// ever finish.
func (c *Controller) Setup(ctx context.Context, input []*KV) error {
	const batchSize = 250

	if err := c.cfg.validateForSetup(); err != nil {
		return err
	}
	if len(input) == 0 {
		return fmt.Errorf("eqmr setup: no input")
	}
	splits := splitInput(input, c.cfg.MapShards)
	store := c.defaultStore()
	if err := store.validate(); err != nil {
		return fmt.Errorf("eqmr setup: %w", err)
	}
	sink := c.sink.Descriptor()
	if err := sink.validate(); err != nil {
		return fmt.Errorf("eqmr setup sink: %w", err)
	}

	// Validate every record before writing anything, rather than letting a bad
	// record surface as a marshaling failure inside a mapper, where it would be
	// reported against a split rather than against the record that is wrong.
	created := 0
	for _, split := range splits {
		if len(split) == 0 {
			continue
		}
		created++
		for j, kv := range split {
			if err := ValidateText(fmt.Sprintf("input key at index %d", j), kv.Key); err != nil {
				return fmt.Errorf("eqmr setup: %w", err)
			}
			if err := ValidateText(fmt.Sprintf("input value at index %d", j), kv.Value); err != nil {
				return fmt.Errorf("eqmr setup: %w", err)
			}
		}
	}

	// Stage the splits. Their deterministic ids make a retry skip what an
	// earlier attempt already wrote instead of writing it twice.
	var staged []entroq.ModifyArg
	flush := func() error {
		if len(staged) == 0 {
			return nil
		}
		if _, err := c.client.Modify(ctx, staged...); err != nil {
			return fmt.Errorf("eqmr setup: stage splits: %w", err)
		}
		staged = staged[:0]
		return nil
	}
	start := []entroq.ModifyArg{entroq.PuttingDocInto(c.DocNS(),
		entroq.WithIDKeys(stateDocID, stateDocKey, ""),
		entroq.WithContent(runState{
			Phase:       PhaseMap,
			MapSplits:   created,
			ReduceParts: c.cfg.ReduceShards,
			Store:       store,
			Sink:        sink,
		}),
	)}
	for i, split := range splits {
		if len(split) == 0 {
			continue
		}
		key := splitDocKey(i)
		if err := ValidText("split doc key", key, MaxDocKeyBytes); err != nil {
			return fmt.Errorf("eqmr setup: %w", err)
		}
		staged = append(staged, entroq.PuttingDocInto(c.DocNS(),
			entroq.WithIDKeys(splitDocID(i), key, ""),
			entroq.WithContent(split),
			entroq.WithSkipCollidingDoc(true),
		))
		if len(staged) >= batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
		start = append(start, entroq.InsertingInto(c.MapQ(), entroq.WithValue(docRef{
			NS:           c.DocNS(),
			Key:          key,
			ReduceShards: c.cfg.ReduceShards,
			Store:        store,
		})))
	}
	if err := flush(); err != nil {
		return err
	}

	// Commit: the run and all of its map tasks appear together.
	_, err := c.client.Modify(ctx, start...)
	if de, ok := entroq.AsDependency(err); ok && de.HasCollisions() {
		return c.alreadySetUp(ctx, created, store, sink)
	}
	if err != nil {
		return fmt.Errorf("eqmr setup: start run: %w", err)
	}
	return nil
}

// alreadySetUp decides whether a Setup whose commit collided with an existing
// state doc is a retry of the Setup that created it, which has nothing left to
// do, or an attempt to reuse a Prefix.
func (c *Controller) alreadySetUp(ctx context.Context, splits int, store, sink Descriptor) error {
	doc, st, err := c.stateDoc(ctx)
	if err != nil {
		return fmt.Errorf("eqmr setup: %w", err)
	}
	if doc != nil && st.MapSplits == splits && st.ReduceParts == c.cfg.ReduceShards && st.Store == store && st.Sink == sink {
		return nil
	}
	return fmt.Errorf("eqmr setup: prefix %q already holds a different run", c.prefix)
}

// splitInput divides input into n contiguous groups whose sizes differ by at
// most one. Groups may be empty when len(input) < n.
func splitInput(input []*KV, n int) [][]*KV {
	splits := make([][]*KV, n)
	if len(input) == 0 {
		return splits
	}
	per, extra := len(input)/n, len(input)%n
	start := 0
	for i := range n {
		size := per
		if i < extra {
			size++
		}
		splits[i] = input[start : start+size]
		start += size
	}
	return splits
}

// MapperOption configures a mapper worker.
type MapperOption func(*mapperOpts)

type mapperOpts struct {
	combiner             Combiner
	intermediateRunBytes int
}

// WithCombiner applies a Combiner to each bounded sorted run before it is
// stored. A split may produce multiple runs, so the Combiner contract permits
// repeated application. See Combiner.
func WithCombiner(cb Combiner) MapperOption {
	return func(o *mapperOpts) { o.combiner = cb }
}

// WithIntermediateRunBytes bounds the mapper memory used to sort one batch of
// intermediate records before writing an immutable run. Non-positive values
// use DefaultIntermediateRunBytes. A single record may exceed the bound.
func WithIntermediateRunBytes(n int) MapperOption {
	return func(o *mapperOpts) { o.intermediateRunBytes = n }
}

// MapperWorker returns a worker that consumes input splits. Run it with
// RunOptions, or use the Run helper for the standard configuration.
//
// The worker reads its split without claiming it, writes immutable runs, and
// then commits a finish task naming the split version it read and the runs it
// wrote, in the same Modify that deletes its own task. It never writes a doc:
// whether this attempt is the one that counts is the controller's decision.
func (c *Controller) MapperWorker(mapFn Mapper, opts ...MapperOption) *worker.Worker[docRef] {
	o := new(mapperOpts)
	for _, opt := range opts {
		opt(o)
	}

	return worker.New[docRef](c.client,
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, ref docRef, _ []*entroq.Doc) (*worker.Result, error) {
			// Read the split WITHOUT claiming it. Claiming would be mutual
			// exclusion at start time, which is exactly what makes a duplicate
			// task pointless: a second worker would block on the claim instead
			// of racing. Exclusion happens when the controller accepts the
			// first finish task for this split version.
			found, err := c.client.Docs(ctx, ref.asQuery())
			if err != nil {
				return nil, retryIfUnavailable(fmt.Errorf("read split %q: %w", ref.Key, err))
			}
			if len(found) == 0 {
				// The split is gone, so the controller already accepted it. This
				// task is the loser of a race, or a straggler whose work was
				// taken over. Either way there is nothing left to do.
				return worker.Modify(task.Delete()), nil
			}
			doc := found[0]

			var split []*KV
			if err := json.Unmarshal(doc.Content, &split); err != nil {
				// A malformed split is a data error, not a transient one:
				// retrying cannot help, and silently skipping it would produce
				// a wrong answer. Quarantine it so the controller fails the run.
				return nil, worker.MoveErrorf("parse split doc %q: %v", ref.Key, err)
			}

			// The partition count comes from the task, not from this process's
			// configuration, so a mapper deployment cannot disagree with the
			// run it is serving.
			if ref.ReduceShards <= 0 {
				return nil, worker.MoveErrorf("map task for %q carries no partition count", ref.Key)
			}
			// The store comes from the task for the same reason. A missing or
			// malformed one is a data error, and one this process cannot reach
			// means the run cannot be completed as deployed. Either way the task
			// is quarantined, and the controller fails the run with this reason
			// so its submitter can fix the deployment and submit again.
			if err := ref.Store.validate(); err != nil {
				return nil, worker.MoveErrorf("map task for %q: %v", ref.Key, err)
			}
			store, err := c.resolveStore(ref.Store)
			if err != nil {
				return nil, worker.MoveErrorf("map task for %q: %v", ref.Key, err)
			}
			out := newIntermediateWriter(store, c.prefix, ref.ReduceShards, o.intermediateRunBytes, o.combiner)
			if err := runMapper(ctx, mapFn, split, out); err != nil {
				abortIntermediate(ctx, out, ref.Key)
				return nil, err
			}
			runs, err := out.finish(ctx)
			if err != nil {
				abortIntermediate(ctx, out, ref.Key)
				return nil, err
			}

			// The runs are durable but unpublished. They become visible only
			// when the controller accepts this finish task, and a finish task
			// exists only if this commit lands. If it does not, the task
			// belongs to someone else now, and the runs are this worker's to
			// remove.
			return worker.Modify(task.Delete(), entroq.InsertingInto(c.MapDoneQ(), entroq.WithValue(mapDone{
				SplitKey: ref.Key,
				Split:    *doc.IDVersion(),
				Runs:     runs,
			}))).OnDependency(func(ctx context.Context, _ *entroq.DependencyError) error {
				abortIntermediate(ctx, out, ref.Key)
				return nil
			}), nil
		}),
	)
}

func abortIntermediate(ctx context.Context, out *intermediateWriter, split string) {
	if err := out.abort(ctx); err != nil {
		log.Printf("eqmr: clean up unpublished runs for split %q: %v", split, err)
	}
}

// runMapper streams every emitted record into out. out cuts sorted durable
// runs as its memory threshold is reached.
func runMapper(ctx context.Context, mapFn Mapper, split []*KV, out *intermediateWriter) error {
	// Validate at the point of emission, so a bad pair names the mapper call
	// that produced it.
	//
	// A MoveError, because text that is not valid UTF-8 stays invalid on a
	// retry: the task is quarantined on the first attempt and the controller
	// fails the run with the reason.
	emit := func(_ context.Context, k, v string) error {
		if err := ValidateText("emitted key", k); err != nil {
			return worker.MoveErrorf("%v", err)
		}
		if err := ValidateText(fmt.Sprintf("value emitted for key %q", k), v); err != nil {
			return worker.MoveErrorf("%v", err)
		}
		return out.write(ctx, k, "", v)
	}

	for _, kv := range split {
		if err := mapFn(ctx, kv.Key, kv.Value, emit); err != nil {
			return fmt.Errorf("map %s: %w", kv, err)
		}
	}
	return out.err
}

// ReducerWorker returns a worker that consumes one reduce partition per task.
//
// It reads every map-output pointer for the partition, heap-merges the
// referenced sorted runs, and runs reduceFn once per distinct key. It then
// commits a finish task carrying the output and the pointers and runs it
// consumed, in the same Modify that deletes its own task. Like a mapper, it
// never writes a doc: the controller records the result.
//
// A run that cannot be opened or read is retried, because a reducer cannot
// tell a transient store failure from inputs that a winning duplicate already
// consumed. A retried loser finds no pointers left and retires; a real failure
// exhausts its attempts and is judged by the controller's error worker.
func (c *Controller) ReducerWorker(reduceFn Reducer) *worker.Worker[reduceTask] {
	return worker.New[reduceTask](c.client,
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, rt reduceTask, _ []*entroq.Doc) (*worker.Result, error) {
			// Read the partition's map outputs WITHOUT claiming them, so a
			// duplicate worker races rather than blocks.
			docs, err := c.client.Docs(ctx, &entroq.DocQuery{
				Namespace: c.DocNS(),
				KeyExact:  mapOutDocKey(rt.Partition),
			})
			if err != nil {
				return nil, retryIfUnavailable(fmt.Errorf("read map outputs for partition %d: %w", rt.Partition, err))
			}
			if len(docs) == 0 {
				// Reduce tasks exist only for partitions with data, so another
				// worker already reduced this one.
				return worker.Modify(task.Delete()), nil
			}

			// The sink is a deployment grant like a store. One this process
			// cannot resolve means the run cannot be completed as deployed:
			// quarantine the task, and the controller fails the run.
			sink, err := c.sinks.resolve(rt.Sink)
			if err != nil {
				return nil, worker.MoveErrorf("partition %d: %v", rt.Partition, err)
			}
			stores := c.stores
			pointers := make([]entroq.DocID, 0, len(docs))
			runs := make([]intermediateRun, 0, len(docs))
			for _, d := range docs {
				run, err := entroq.GetContent[intermediateRun](d)
				if err != nil {
					return nil, worker.MoveErrorf("parse map-output pointer %q: %v", d.ID, err)
				}
				if run.Partition != rt.Partition {
					return nil, worker.MoveErrorf("map-output pointer %q names partition %d, want %d", d.ID, run.Partition, rt.Partition)
				}
				if _, err := stores.resolve(run.Store); err != nil {
					return nil, worker.MoveErrorf("partition %d: %v", rt.Partition, err)
				}
				pointers = append(pointers, *d.IDVersion())
				runs = append(runs, run)
			}

			merged, err := openMergedIntermediate(ctx, stores, runs)
			if err != nil {
				return nil, runReadFailure(err, "open partition %d", rt.Partition)
			}
			writer, err := sink.Stage(ctx, c.prefix, rt.Partition)
			if err != nil {
				return nil, errors.Join(sinkFailure(err, "stage partition %d", rt.Partition), merged.Close())
			}
			reduceErr := reduceIntermediate(ctx, merged, reduceFn, writer)
			mergeErr := merged.Close()
			staged, stageErr := writer.Close(ctx)
			if err := errors.Join(reduceErr, mergeErr); err != nil {
				return nil, err
			}
			if stageErr != nil {
				return nil, sinkFailure(stageErr, "stage partition %d", rt.Partition)
			}

			// The output is staged, not committed: only the controller decides
			// which attempt at this partition becomes the output. An attempt
			// that no finish task ends up naming, because this commit did not
			// land or the reduce failed above, is uniquely named garbage, and
			// sweeping the sink at the end of the run collects it.
			return worker.Modify(task.Delete(), entroq.InsertingInto(c.ReduceDoneQ(), entroq.WithValue(reduceDone{
				Partition: rt.Partition,
				Pointers:  pointers,
				Runs:      runs,
				Sink:      rt.Sink,
				Staged:    staged,
			}))), nil
		}),
	)
}

// sinkFailure classifies a failure reported by a sink. A sink that knows no
// retry can fix it says so with a worker.MoveError, and the task is
// quarantined, failing the run with the sink's reason; anything else is
// retried, bounded by the claim ceiling.
func sinkFailure(err error, format string, args ...any) error {
	what := fmt.Sprintf(format, args...)
	if _, ok := worker.AsMove(err); ok {
		return worker.MoveErrorf("%s: %v", what, err)
	}
	return worker.RetryErrorf("%s: %v", what, err)
}

// runReadFailure classifies a failure to read intermediate runs. A corrupt run
// will read the same way every time, so its task is quarantined at once and
// the run fails with the reason. Anything else is retried: a run that is
// missing may have been collected after another worker reduced the partition,
// and one that ends early may have lost its connection; either way the claim
// ceiling bounds the retries.
func runReadFailure(err error, format string, args ...any) error {
	what := fmt.Sprintf(format, args...)
	if errors.Is(err, errCorruptRun) {
		return worker.MoveErrorf("%s: %v", what, err)
	}
	return worker.RetryErrorf("%s: %v", what, err)
}

// reduceIntermediate runs reduceFn once per key of merged, in key order, and
// writes each result to out as it is produced.
func reduceIntermediate(ctx context.Context, merged *mergedIntermediate, reduceFn Reducer, out SinkWriter) error {
	stream := newGroupedIntermediate(ctx, merged)
	for {
		input, ok := stream.nextGroup()
		if !ok {
			if stream.err != nil {
				return runReadFailure(stream.err, "read intermediate records")
			}
			return nil
		}
		result, err := reduceFn(ctx, input)
		if err != nil {
			return fmt.Errorf("reduce %q: %w", input.Key(), err)
		}
		if err := input.drain(); err != nil {
			return runReadFailure(err, "drain reduce input %q", input.Key())
		}
		if err := out.Write(ctx, input.Key(), result); err != nil {
			return sinkFailure(err, "write output for %q", input.Key())
		}
	}
}
