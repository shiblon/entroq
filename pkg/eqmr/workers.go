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
	// Sink is the run's sink, stamped from the run state.
	Sink Descriptor `json:"sink"`
}

// Setup divides input into Config.MapShards contiguous splits and creates the
// run, with one split doc and one map task per non-empty split.
//
// It stages the split docs under deterministic ids, then creates the state doc
// and every map task in one Modify, so a run is never half set up. Setup may be
// retried with the same input. A Prefix that holds a different run is an
// error, and so is empty input.
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

	// Validate every record first, so an error names the record.
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

	// Deterministic ids let a retry skip splits already staged.
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

	// The run and all of its map tasks appear together.
	_, err := c.client.Modify(ctx, start...)
	if de, ok := entroq.AsDependency(err); ok && de.HasCollisions() {
		return c.alreadySetUp(ctx, created, store, sink)
	}
	if err != nil {
		return fmt.Errorf("eqmr setup: start run: %w", err)
	}
	return nil
}

// alreadySetUp reports whether a commit that collided with an existing state
// doc was a retry of the Setup that created it.
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

// WithCombiner applies cb to each sorted run before it is stored. A split may
// produce several runs, so cb may apply more than once; see Combiner.
func WithCombiner(cb Combiner) MapperOption {
	return func(o *mapperOpts) { o.combiner = cb }
}

// WithIntermediateRunBytes bounds the mapper memory used to sort one batch of
// intermediate records before writing an immutable run. Non-positive values
// use DefaultIntermediateRunBytes. A single record may exceed the bound.
func WithIntermediateRunBytes(n int) MapperOption {
	return func(o *mapperOpts) { o.intermediateRunBytes = n }
}

// MapperWorker returns a worker for map tasks. It reads its split without
// claiming it, writes runs, and commits a finish task naming the split it read
// and the runs it wrote, in the Modify that deletes its task. RunMapper runs it
// with the run's options.
func (c *Controller) MapperWorker(mapFn Mapper, opts ...MapperOption) *worker.Worker[docRef] {
	o := new(mapperOpts)
	for _, opt := range opts {
		opt(o)
	}

	return worker.New[docRef](c.client,
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, ref docRef, _ []*entroq.Doc) (*worker.Result, error) {
			// Not claiming the split lets a duplicate task race this one.
			found, err := c.client.Docs(ctx, ref.asQuery())
			if err != nil {
				return nil, retryIfUnavailable(fmt.Errorf("read split %q: %w", ref.Key, err))
			}
			if len(found) == 0 {
				// The split was already accepted.
				return worker.Modify(task.Delete()), nil
			}
			doc := found[0]

			var split []*KV
			if err := json.Unmarshal(doc.Content, &split); err != nil {
				// A malformed split cannot succeed on a retry.
				return nil, worker.MoveErrorf("parse split doc %q: %v", ref.Key, err)
			}

			// The partition count and store come from the task, so a mapper
			// cannot disagree with its run. A store that is missing, malformed,
			// or unreachable fails the run.
			if ref.ReduceShards <= 0 {
				return nil, worker.MoveErrorf("map task for %q carries no partition count", ref.Key)
			}
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

			// The runs are unpublished until the controller accepts this finish.
			// If this commit fails, the task is someone else's, and the runs are
			// this worker's to remove.
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

// runMapper streams every emitted record into out, which cuts sorted runs as
// its buffer fills.
func runMapper(ctx context.Context, mapFn Mapper, split []*KV, out *intermediateWriter) error {
	// Text is checked where it is emitted, so an error names the mapper call.
	// Invalid text cannot succeed on a retry.
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

// ReducerWorker returns a worker for reduce tasks. It merges the runs named by
// its partition's pointers, calls reduceFn once per key, streams the results
// into a staged attempt at the run's sink, and commits a finish task naming
// the attempt and what it consumed, in the Modify that deletes its task.
//
// A run that cannot be read is retried: a loser whose inputs were collected
// finds no pointers on the retry and retires.
func (c *Controller) ReducerWorker(reduceFn Reducer) *worker.Worker[reduceTask] {
	return worker.New[reduceTask](c.client,
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, rt reduceTask, _ []*entroq.Doc) (*worker.Result, error) {
			// Not claiming the pointers lets a duplicate task race this one.
			docs, err := c.client.Docs(ctx, &entroq.DocQuery{
				Namespace: c.DocNS(),
				KeyExact:  mapOutDocKey(rt.Partition),
			})
			if err != nil {
				return nil, retryIfUnavailable(fmt.Errorf("read map outputs for partition %d: %w", rt.Partition, err))
			}
			if len(docs) == 0 {
				// Already reduced: reduce tasks exist only for partitions with
				// data.
				return worker.Modify(task.Delete()), nil
			}

			// A sink or store this process cannot reach fails the run.
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

			// The controller decides which attempt is committed. One that no
			// finish names is swept with the sink.
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

// sinkFailure quarantines the task on a worker.MoveError from the sink, which
// fails the run, and retries anything else.
func sinkFailure(err error, format string, args ...any) error {
	what := fmt.Sprintf(format, args...)
	if _, ok := worker.AsMove(err); ok {
		return worker.MoveErrorf("%s: %v", what, err)
	}
	return worker.RetryErrorf("%s: %v", what, err)
}

// runReadFailure quarantines the task on a corrupt run, which reads the same
// every time, and retries anything else: a missing run may have been collected
// after another worker reduced the partition.
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
