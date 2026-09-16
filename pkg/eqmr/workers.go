package eqmr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

// reduceClaim is the value of a reduce task: one partition of the shuffle.
type reduceClaim struct {
	Doc       docRef `json:"doc"`
	Partition int    `json:"partition"`
}

// Run phases, carried in the control task's value.
const (
	PhaseMap    = "map"
	PhaseReduce = "reduce"
	PhaseDone   = "done"
	PhaseFailed = "failed"
)

// terminalRearm is how far into the future a finished or failed control task is
// pushed. It is kept rather than deleted so the outcome stays inspectable with
// ordinary task tooling until Cleanup runs.
const terminalRearm = 24 * time.Hour

// controlState is the value of the single control task. The task is both the
// phase state and the timer, so the state advances atomically with the requeue.
type controlState struct {
	Phase string `json:"phase"`
	// MapSplits is the number of split docs Setup actually wrote. Empty splits
	// are skipped, so this can be less than Config.MapShards, and only Setup
	// knows the true figure.
	MapSplits int `json:"map_splits"`
	// ReduceParts is the partition count the run was created with, stamped so
	// progress reporting does not depend on a caller's live config matching.
	ReduceParts int `json:"reduce_parts"`
	// LastProgress is when the run last looked different from the check before.
	LastProgress time.Time `json:"last_progress"`
	// LastRemaining is the count of outstanding units at the previous check.
	LastRemaining int `json:"last_remaining"`
	// Reason explains a PhaseFailed outcome.
	Reason string `json:"reason,omitempty"`
}

// Setup writes the input splits and starts the run. It divides input into
// Config.MapShards contiguous splits, writes one split doc and one map task per
// non-empty split, and creates the single control task that drives the phases.
//
// Setup is not idempotent: a second call against the same Prefix collides on
// the split doc keys. Use a fresh Prefix per run.
func (c *Controller) Setup(ctx context.Context, input []*KV) error {
	const batchSize = 250

	if err := c.cfg.validateForSetup(); err != nil {
		return err
	}
	splits := splitInput(input, c.cfg.MapShards)

	// Empty splits are never written, so the number of map units is not
	// necessarily MapShards. Progress reporting needs the real total, and it is
	// only knowable here, so it is stamped into the control task rather than
	// inferred later from a config value that may overstate it.
	created := 0
	for _, split := range splits {
		if len(split) > 0 {
			created++
		}
	}

	var args []entroq.ModifyArg
	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		if _, err := c.client.Modify(ctx, args...); err != nil {
			return fmt.Errorf("eqmr setup: %w", err)
		}
		args = args[:0]
		return nil
	}

	for i, split := range splits {
		if len(split) == 0 {
			continue
		}
		// Validate the input here rather than letting a bad record surface as a
		// marshaling failure inside a mapper, where it would be reported against
		// a split rather than against the record that is actually wrong.
		for j, kv := range split {
			if err := ValidateText(fmt.Sprintf("input key at index %d", j), kv.Key); err != nil {
				return fmt.Errorf("eqmr setup: %w", err)
			}
			if err := ValidateText(fmt.Sprintf("input value at index %d", j), kv.Value); err != nil {
				return fmt.Errorf("eqmr setup: %w", err)
			}
		}
		key := splitDocKey(i)
		if err := ValidText("split doc key", key, MaxDocKeyBytes); err != nil {
			return fmt.Errorf("eqmr setup: %w", err)
		}
		args = append(args,
			entroq.PuttingDocInto(c.DocNS(), entroq.WithKeys(key, ""), entroq.WithContent(split)),
			entroq.InsertingInto(c.MapQ(), entroq.WithValue(docRef{
				NS:           c.DocNS(),
				Key:          key,
				ReduceShards: c.cfg.ReduceShards,
			})),
		)
		if len(args) >= batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}

	// The control task is created last, so a failed Setup never leaves a
	// controller polling a run whose inputs were never fully written.
	if _, err := c.client.Modify(ctx, entroq.InsertingInto(c.ControlQ(),
		entroq.WithValue(controlState{
			Phase:        PhaseMap,
			MapSplits:    created,
			ReduceParts:  c.cfg.ReduceShards,
			LastProgress: time.Now(),
		}),
	)); err != nil {
		return fmt.Errorf("eqmr setup control task: %w", err)
	}
	return nil
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
// The worker reads its split without claiming it, writes immutable run payloads,
// then commits the split deletion in the same Modify that publishes their
// pointer docs. That atomic publication makes "no split docs remain" a sound
// completion barrier.
func (c *Controller) MapperWorker(mapFn Mapper, opts ...MapperOption) *worker.Worker[docRef] {
	o := new(mapperOpts)
	for _, opt := range opts {
		opt(o)
	}

	store := c.intermediateStore()
	return worker.New[docRef](c.client,
		worker.WithErrQMap[docRef](c.errQMap),
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, ref docRef, _ []*entroq.Doc) (*worker.Result, error) {
			// Read the split WITHOUT claiming it. Claiming would be mutual
			// exclusion at start time, which is exactly what makes a duplicate
			// task pointless: a second worker would block on the claim instead
			// of racing. Exclusion happens at commit time instead, through the
			// version-pinned delete below, so two workers may process the same
			// split and the first to commit wins.
			found, err := c.client.Docs(ctx, ref.asQuery())
			if err != nil {
				return nil, fmt.Errorf("read split %q: %w", ref.Key, err)
			}
			if len(found) == 0 {
				// The split is gone, so someone else already committed it. This
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
			sink := newIntermediateSink(store, ref.ReduceShards, o.intermediateRunBytes, o.combiner)
			if err := runMapper(ctx, mapFn, split, sink); err != nil {
				abortIntermediate(ctx, sink, ref.Key)
				return nil, err
			}
			runs, err := sink.finish(ctx)
			if err != nil {
				abortIntermediate(ctx, sink, ref.Key)
				return nil, err
			}

			// doc.Delete() is pinned to the version read above, so this whole
			// modification is rejected if another worker committed the same
			// split first. Run payloads are durable already, while the pointer docs
			// below are published atomically with this deletion. A losing worker
			// removes its unpublished payloads in OnDependency.
			modArgs := []entroq.ModifyArg{task.Delete(), doc.Delete()}
			for _, run := range runs {
				modArgs = append(modArgs, entroq.PuttingDocInto(c.DocNS(),
					entroq.WithKeys(mapOutDocKey(run.Partition), ""),
					entroq.WithContent(run),
				))
			}
			return worker.Modify(modArgs...).OnDependency(func(ctx context.Context, de *entroq.DependencyError) error {
				abortIntermediate(ctx, sink, ref.Key)
				return lostRace(ctx, de)
			}), nil
		}),
	)
}

func abortIntermediate(ctx context.Context, sink *intermediateSink, split string) {
	if err := sink.abort(ctx); err != nil {
		log.Printf("eqmr: clean up unpublished runs for split %q: %v", split, err)
	}
}

// lostRace is the disposition for a commit that failed on a dependency.
//
// A nil return leaves the task alone. It is reclaimed on lease expiry, re-reads
// its input, finds the work already done and retires. Never a RetryError: that
// increments the attempt count, and a worker that lost a race must not be
// quarantined, since quarantining anything fails the run.
//
// Unclassified failures stop the worker, per the OnDependency contract.
func lostRace(_ context.Context, de *entroq.DependencyError) error {
	switch {
	case de.HasMissingDocs():
		// The input document is gone: another worker committed this unit first.
		// This is the ordinary way to lose a speculative race.
		return nil

	case de.HasCollisions():
		// An explicit-id insert collided, meaning another worker already
		// recorded this partition's result. It is how an empty partition loses,
		// having no input document to lose instead.
		return nil

	case de.HasMissing() || de.HasClaims():
		// Our own task was deleted or reclaimed while we worked, so it belongs
		// to someone else now. Ordinary lease behavior, nothing to dispose of.
		return nil

	default:
		// Nothing in this package claims documents, so document contention here
		// means an assumption no longer holds. Stop and let the orchestrator
		// restart rather than continue from an unexplained failure.
		return fmt.Errorf("eqmr: unexpected dependency failure: %w", de)
	}
}

// runMapper streams every emitted record into sink. sink cuts sorted durable
// runs as its memory threshold is reached.
func runMapper(ctx context.Context, mapFn Mapper, split []*KV, sink *intermediateSink) error {
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
		return sink.write(ctx, k, "", v)
	}

	for _, kv := range split {
		if err := mapFn(ctx, kv.Key, kv.Value, emit); err != nil {
			return fmt.Errorf("map %s: %w", kv, err)
		}
	}
	return sink.err
}

// ReducerWorker returns a worker that consumes one reduce partition per task.
//
// It reads every map-output pointer for the partition, heap-merges the referenced
// sorted runs, runs reduceFn once per distinct key, and commits the pointer
// deletions together with the partition's result doc.
func (c *Controller) ReducerWorker(reduceFn Reducer) *worker.Worker[reduceClaim] {
	store := c.intermediateStore()
	return worker.New[reduceClaim](c.client,
		worker.WithErrQMap[reduceClaim](c.errQMap),
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, rc reduceClaim, _ []*entroq.Doc) (*worker.Result, error) {
			// Read the partition's map outputs WITHOUT claiming them, for the same
			// reason the mapper does: a claim would block a duplicate worker
			// rather than race it. Exclusion is at commit time.
			docs, err := c.client.Docs(ctx, rc.Doc.asQuery())
			if err != nil {
				return nil, fmt.Errorf("read map outputs %q: %w", rc.Doc.Key, err)
			}

			if len(docs) == 0 {
				// Either nothing was ever emitted to this partition, or another
				// worker already reduced it. Those look identical from here, so
				// ask whether the output exists.
				done, err := c.client.Docs(ctx, &entroq.DocQuery{
					Namespace:  c.DocNS(),
					KeyExact:   resultDocKey(rc.Partition),
					OmitValues: true,
				})
				if err != nil {
					return nil, fmt.Errorf("check result %d: %w", rc.Partition, err)
				}
				if len(done) > 0 {
					return worker.Modify(task.Delete()), nil
				}
				// Genuinely empty. Write an empty result anyway, so that "this
				// partition is finished" is a recorded fact rather than an
				// absence: completion is judged purely from documents, and a
				// silent partition would be indistinguishable from one that
				// never ran. The explicit document id makes a simultaneous
				// duplicate collide rather than write a second result.
				return worker.Modify(task.Delete(), c.resultInsert(rc.Partition, []*KV{})).
					OnDependency(lostRace), nil
			}

			runs := make([]intermediateRun, 0, len(docs))
			for _, d := range docs {
				var run intermediateRun
				if err := json.Unmarshal(d.Content, &run); err != nil {
					return nil, worker.MoveErrorf("parse map-output pointer %q: %v", d.ID, err)
				}
				if run.Partition != rc.Partition {
					return nil, worker.MoveErrorf("map-output pointer %q names partition %d, want %d", d.ID, run.Partition, rc.Partition)
				}
				runs = append(runs, run)
			}

			merged, err := openMergedIntermediate(ctx, store, runs)
			if err != nil {
				return nil, fmt.Errorf("open partition %d: %w", rc.Partition, err)
			}
			out, reduceErr := reduceIntermediate(ctx, merged, reduceFn)
			closeErr := merged.Close()
			if err := errors.Join(reduceErr, closeErr); err != nil {
				return nil, err
			}

			// Every map output delete is version-pinned, so a worker that lost this
			// partition to another has its whole modification rejected and
			// writes no result.
			modArgs := []entroq.ModifyArg{task.Delete()}
			for _, d := range docs {
				modArgs = append(modArgs, d.Delete())
			}
			modArgs = append(modArgs, c.resultInsert(rc.Partition, out))
			return worker.Modify(modArgs...).OnDependency(lostRace), nil
		}),
	)
}

func reduceIntermediate(ctx context.Context, merged *mergedIntermediate, reduceFn Reducer) ([]*KV, error) {
	stream := newGroupedIntermediate(ctx, merged)
	var out []*KV
	for {
		input, ok := stream.nextGroup()
		if !ok {
			if stream.err != nil {
				return nil, fmt.Errorf("read intermediate records: %w", stream.err)
			}
			return out, nil
		}
		result, err := reduceFn(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("reduce %q: %w", input.Key(), err)
		}
		if err := input.drain(); err != nil {
			return nil, fmt.Errorf("drain reduce input %q: %w", input.Key(), err)
		}
		out = append(out, NewKV(input.Key(), result))
	}
}

// resultInsert builds the insert for one partition's output. The explicit
// document id is what stops two racing workers from both recording a result.
func (c *Controller) resultInsert(partition int, out []*KV) entroq.ModifyArg {
	return entroq.PuttingDocInto(c.DocNS(),
		entroq.WithIDKeys(resultDocID(partition), resultDocKey(partition), ""),
		entroq.WithContent(out),
	)
}
