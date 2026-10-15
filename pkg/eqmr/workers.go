package eqmr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// run from them; see SetupFrom. Empty input is an error.
func (c *Controller) Setup(ctx context.Context, input []*KV) error {
	if c.cfg.MapShards <= 0 {
		return fmt.Errorf("eqmr setup: MapShards must be set with WithMapShards; it fixes how input is divided")
	}
	// Validate every record first, so an error names the record.
	for i, kv := range input {
		if err := ValidateText(fmt.Sprintf("input key at index %d", i), kv.Key); err != nil {
			return fmt.Errorf("eqmr setup: %w", err)
		}
		if err := ValidateText(fmt.Sprintf("input value at index %d", i), kv.Value); err != nil {
			return fmt.Errorf("eqmr setup: %w", err)
		}
	}
	return c.setup(ctx, c.inlineSource(input, c.cfg.MapShards))
}

// SetupFrom creates the run with one split doc and one map task per split of
// the source set by WithSource. MapShards does not apply, and must be unset.
//
// It stages the split docs under deterministic ids, then creates the state doc
// and every map task in one Modify, so a run is never half set up. SetupFrom
// may be retried with the same source. A Prefix that holds a different run is
// an error, and so is a source with no splits.
func (c *Controller) SetupFrom(ctx context.Context) error {
	if c.cfg.Source == nil {
		return fmt.Errorf("eqmr setup: no source; configure one with WithSource")
	}
	if c.cfg.MapShards != 0 {
		return fmt.Errorf("eqmr setup: MapShards applies only to Setup; a source's splits are its own")
	}
	return c.setup(ctx, c.cfg.Source)
}

func (c *Controller) setup(ctx context.Context, src Source) error {
	const batchSize = 250

	if c.cfg.ReduceShards <= 0 {
		return fmt.Errorf("eqmr setup: ReduceShards must be set with WithReduceShards; it fixes the document layout when the map phase writes its output")
	}
	source := src.Descriptor()
	if err := source.validate(); err != nil {
		return fmt.Errorf("eqmr setup source: %w", err)
	}
	store := c.defaultStore()
	if err := store.validate(); err != nil {
		return fmt.Errorf("eqmr setup: %w", err)
	}
	sink := c.sink.Descriptor()
	if err := sink.validate(); err != nil {
		return fmt.Errorf("eqmr setup sink: %w", err)
	}
	splits, err := src.Splits(ctx)
	if err != nil {
		return fmt.Errorf("eqmr setup: list splits: %w", err)
	}
	if len(splits) == 0 {
		return fmt.Errorf("eqmr setup: no input")
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
	st := runState{
		Phase:       PhaseMap,
		MapSplits:   len(splits),
		ReduceParts: c.cfg.ReduceShards,
		Source:      source,
		Store:       store,
		Sink:        sink,
	}
	start := []entroq.ModifyArg{entroq.PuttingDocInto(c.DocNS(),
		entroq.WithIDKeys(stateDocID, stateDocKey, ""),
		entroq.WithContent(st),
	)}
	for i, split := range splits {
		key := splitDocKey(i)
		staged = append(staged, entroq.PuttingDocInto(c.DocNS(),
			entroq.WithIDKeys(splitDocID(i), key, ""),
			entroq.WithContent(splitDoc{Split: split}),
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
			Source:       source,
			Store:        store,
		})))
	}
	if err := flush(); err != nil {
		return err
	}

	// The run and all of its map tasks appear together.
	_, err = c.client.Modify(ctx, start...)
	if de, ok := entroq.AsDependency(err); ok && de.HasCollisions() {
		return c.alreadySetUp(ctx, st)
	}
	if err != nil {
		return fmt.Errorf("eqmr setup: start run: %w", err)
	}
	return nil
}

// alreadySetUp reports whether a commit that collided with an existing state
// doc was a retry of the Setup that created it.
func (c *Controller) alreadySetUp(ctx context.Context, want runState) error {
	doc, st, err := c.stateDoc(ctx)
	if err != nil {
		return fmt.Errorf("eqmr setup: %w", err)
	}
	if doc != nil && st.MapSplits == want.MapSplits && st.ReduceParts == want.ReduceParts &&
		st.Source == want.Source && st.Store == want.Store && st.Sink == want.Sink {
		return nil
	}
	return fmt.Errorf("eqmr setup: prefix %q already holds a different run", c.prefix)
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

			var split splitDoc
			if err := json.Unmarshal(doc.Content, &split); err != nil {
				// A malformed split cannot succeed on a retry.
				return nil, worker.MoveErrorf("parse split doc %q: %v", ref.Key, err)
			}

			// The partition count, source, and store come from the task, so a
			// mapper cannot disagree with its run. A source or store that is
			// missing, malformed, or unreachable fails the run.
			if ref.ReduceShards <= 0 {
				return nil, worker.MoveErrorf("map task for %q carries no partition count", ref.Key)
			}
			for _, d := range []Descriptor{ref.Source, ref.Store} {
				if err := d.validate(); err != nil {
					return nil, worker.MoveErrorf("map task for %q: %v", ref.Key, err)
				}
			}
			src, err := c.sources.resolve(ref.Source)
			if err != nil {
				return nil, worker.MoveErrorf("map task for %q: %v", ref.Key, err)
			}
			store, err := c.resolveStore(ref.Store)
			if err != nil {
				return nil, worker.MoveErrorf("map task for %q: %v", ref.Key, err)
			}
			records, err := src.Open(ctx, split.Split)
			if err != nil {
				return nil, moveOrRetry(err, "open split %q", ref.Key)
			}
			defer records.Close()
			out := newIntermediateWriter(store, c.prefix, ref.ReduceShards, o.intermediateRunBytes, o.combiner)
			if err := runMapper(ctx, mapFn, records, out); err != nil {
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

// runMapper calls mapFn on every record and streams what it emits into out,
// which cuts sorted runs as its buffer fills.
func runMapper(ctx context.Context, mapFn Mapper, records Records, out *intermediateWriter) error {
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

	for {
		kv, err := records.Next(ctx)
		if err == io.EOF {
			return out.err
		}
		if err != nil {
			return moveOrRetry(err, "read input")
		}
		if err := ValidateText("input key", kv.Key); err != nil {
			return worker.MoveErrorf("%v", err)
		}
		if err := ValidateText(fmt.Sprintf("input value for key %q", kv.Key), kv.Value); err != nil {
			return worker.MoveErrorf("%v", err)
		}
		if err := mapFn(ctx, kv.Key, kv.Value, emit); err != nil {
			return fmt.Errorf("map %s: %w", kv, err)
		}
	}
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
				return nil, errors.Join(moveOrRetry(err, "stage partition %d", rt.Partition), merged.Close())
			}
			reduceErr := reduceIntermediate(ctx, merged, reduceFn, writer)
			mergeErr := merged.Close()
			staged, stageErr := writer.Close(ctx)
			if err := errors.Join(reduceErr, mergeErr); err != nil {
				return nil, err
			}
			if stageErr != nil {
				return nil, moveOrRetry(stageErr, "stage partition %d", rt.Partition)
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

// moveOrRetry quarantines the task on a worker.MoveError from a source or sink,
// which fails the run, and retries anything else.
func moveOrRetry(err error, format string, args ...any) error {
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
			return moveOrRetry(err, "write output for %q", input.Key())
		}
	}
}
