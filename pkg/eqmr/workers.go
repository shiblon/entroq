package eqmr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

// keyValues is one intermediate key with all of its values from a single map
// split. A spill doc holds a slice of these, sorted by key, which is what makes
// the reducer's merge a linear k-way merge rather than a full sort.
type keyValues struct {
	Key    []byte   `json:"key"`
	Values [][]byte `json:"values"`
}

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
		key := splitDocKey(i)
		if err := ValidText("split doc key", key, MaxDocKeyBytes); err != nil {
			return fmt.Errorf("eqmr setup: %w", err)
		}
		args = append(args,
			entroq.PuttingDocInto(c.DocNS(), entroq.WithKeys(key, ""), entroq.WithContent(split)),
			entroq.InsertingInto(c.MapQ(), entroq.WithValue(docRef{NS: c.DocNS(), Key: key})),
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
	combiner Combiner
}

// WithCombiner applies a Combiner to each key's values before they are written
// to spill docs. This is the cheapest place to combine and usually the most
// effective, because it shrinks the data before it is ever stored. See Combiner
// for the contract it must satisfy.
func WithCombiner(cb Combiner) MapperOption {
	return func(o *mapperOpts) { o.combiner = cb }
}

// MapperWorker returns a worker that consumes input splits. Run it with
// RunOptions, or use the Run helper for the standard configuration.
//
// The worker claims its split doc before working, and commits the split doc's
// deletion in the same Modify that writes its spill docs. That single atomic
// step is what makes "no split docs remain" a sound completion barrier.
func (c *Controller) MapperWorker(mapFn Mapper, opts ...MapperOption) *worker.Worker[docRef] {
	o := new(mapperOpts)
	for _, opt := range opts {
		opt(o)
	}

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

			grouped, order, err := runMapper(ctx, mapFn, split)
			if err != nil {
				return nil, err
			}
			if o.combiner != nil {
				for _, k := range order {
					combined, err := o.combiner(ctx, []byte(k), grouped[k])
					if err != nil {
						return nil, fmt.Errorf("combine %q: %w", k, err)
					}
					grouped[k] = combined
				}
			}

			buckets := partition(grouped, order, c.cfg.ReduceShards)

			// doc.Delete() is pinned to the version read above, so this whole
			// modification is rejected if another worker committed the same
			// split first. Rejection is atomic: the losing worker's spill docs
			// are never written, so a race costs duplicated compute and nothing
			// else.
			modArgs := []entroq.ModifyArg{task.Delete(), doc.Delete()}
			for p, entries := range buckets {
				if len(entries) == 0 {
					continue
				}
				modArgs = append(modArgs, entroq.PuttingDocInto(c.DocNS(),
					// No secondary key. A secondary key keeps subsets of a
					// primary-key group located together; it does not define
					// the group, which here is the whole partition and is taken
					// in one ClaimDocs. This partition has no subset that needs
					// co-locating: the merge is order-independent and the
					// reducer sorts values itself. Left free rather than
					// occupied by something nothing reads, so a later job-
					// specific use (MapReduce's classic one being secondary
					// sort) still has it.
					entroq.WithKeys(spillDocKey(p), ""),
					entroq.WithContent(entries),
				))
			}
			return worker.Modify(modArgs...).OnDependency(lostRace), nil
		}),
	)
}

// lostRace is the disposition for a commit that failed on a dependency.
//
// Every branch that returns nil leaves the task alone, to be reclaimed on lease
// expiry, at which point it re-reads its input, finds the work already done, and
// retires. None of them is a RetryError: a retry increments the attempt count,
// and a worker whose only misfortune was losing a race must never be
// quarantined, because quarantining anything fails the whole run.
//
// The default case is the point of the switch. Per the OnDependency contract,
// failures you understand are classified and anything else stops the worker,
// rather than continuing from a state whose cause is unknown.
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
		// to someone else now. Ordinary lease behaviour, nothing to dispose of.
		return nil

	default:
		// Nothing in this package claims documents, so document contention here
		// means an assumption no longer holds. Stop and let the orchestrator
		// restart rather than continue from an unexplained failure.
		return fmt.Errorf("eqmr: unexpected dependency failure: %w", de)
	}
}

// runMapper runs mapFn over every KV in a split, collecting emitted pairs
// grouped by key and remembering first-emit order for deterministic bucketing.
func runMapper(ctx context.Context, mapFn Mapper, split []*KV) (map[string][][]byte, []string, error) {
	grouped := make(map[string][][]byte)
	var order []string

	emit := func(_ context.Context, k, v []byte) error {
		ks := string(k)
		if _, seen := grouped[ks]; !seen {
			order = append(order, ks)
		}
		grouped[ks] = append(grouped[ks], v)
		return nil
	}

	for _, kv := range split {
		if err := mapFn(ctx, kv.Key, kv.Value, emit); err != nil {
			return nil, nil, fmt.Errorf("map %s: %w", kv, err)
		}
	}
	return grouped, order, nil
}

// partition assigns each key to a reduce shard and returns per-shard entries
// sorted by key, which is the precondition for the reducer's merge.
func partition(grouped map[string][][]byte, order []string, shards int) [][]*keyValues {
	buckets := make([][]*keyValues, shards)
	for _, ks := range order {
		key := []byte(ks)
		p := ShardForKey(key, shards)
		vals := grouped[ks]
		sortValues(vals)
		buckets[p] = append(buckets[p], &keyValues{Key: key, Values: vals})
	}
	for _, entries := range buckets {
		sort.Slice(entries, func(i, j int) bool {
			return bytes.Compare(entries[i].Key, entries[j].Key) < 0
		})
	}
	return buckets
}

func sortValues(vals [][]byte) {
	sort.Slice(vals, func(i, j int) bool { return bytes.Compare(vals[i], vals[j]) < 0 })
}

// ReducerWorker returns a worker that consumes one reduce partition per task.
//
// It claims every spill doc sharing the partition's primary key in a single
// ClaimDocs, merges those sorted runs, runs reduceFn once per distinct key, and
// commits the spill deletions together with the partition's result doc. As in
// the map phase, that atomicity is what makes "no spill docs remain" a sound
// barrier.
func (c *Controller) ReducerWorker(reduceFn Reducer) *worker.Worker[reduceClaim] {
	return worker.New[reduceClaim](c.client,
		worker.WithErrQMap[reduceClaim](c.errQMap),
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, rc reduceClaim, _ []*entroq.Doc) (*worker.Result, error) {
			// Read the partition's spills WITHOUT claiming them, for the same
			// reason the mapper does: a claim would block a duplicate worker
			// rather than race it. Exclusion is at commit time.
			docs, err := c.client.Docs(ctx, rc.Doc.asQuery())
			if err != nil {
				return nil, fmt.Errorf("read spills %q: %w", rc.Doc.Key, err)
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

			runs := make([][]*keyValues, 0, len(docs))
			for _, d := range docs {
				var entries []*keyValues
				if err := json.Unmarshal(d.Content, &entries); err != nil {
					return nil, worker.MoveErrorf("parse spill doc %q: %v", d.ID, err)
				}
				runs = append(runs, entries)
			}

			var out []*KV
			err = mergeRuns(runs, func(key []byte, values [][]byte) error {
				// Values arrive as the concatenation of per-split runs, each
				// internally sorted. Sorting again makes a key's reduce input
				// independent of split count and combine order, so results are
				// reproducible across differently sharded runs of the same input.
				sortValues(values)
				result, err := reduceFn(ctx, &sliceInput{key: key, values: values})
				if err != nil {
					return fmt.Errorf("reduce %q: %w", key, err)
				}
				out = append(out, NewKV(key, result))
				return nil
			})
			if err != nil {
				return nil, err
			}

			// Every spill delete is version-pinned, so a worker that lost this
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

// resultInsert builds the insert for one partition's output. The explicit
// document id is what stops two racing workers from both recording a result.
func (c *Controller) resultInsert(partition int, out []*KV) entroq.ModifyArg {
	return entroq.PuttingDocInto(c.DocNS(),
		entroq.WithIDKeys(resultDocID(partition), resultDocKey(partition), ""),
		entroq.WithContent(out),
	)
}

// mergeRuns performs a k-way merge over runs that are each sorted by key,
// calling visit once per distinct key with every value for it, in key order.
//
// Cursor-per-run with a linear minimum scan is O(k) per key. With k equal to
// the number of map splits that is comfortably cheaper than the reduce work
// itself; swapping in a heap becomes worthwhile only once spills are read
// lazily rather than unmarshaled up front.
func mergeRuns(runs [][]*keyValues, visit func(key []byte, values [][]byte) error) error {
	cursors := make([]int, len(runs))
	for {
		var minKey []byte
		for i, run := range runs {
			if cursors[i] >= len(run) {
				continue
			}
			if minKey == nil || bytes.Compare(run[cursors[i]].Key, minKey) < 0 {
				minKey = run[cursors[i]].Key
			}
		}
		if minKey == nil {
			return nil
		}

		var values [][]byte
		for i, run := range runs {
			for cursors[i] < len(run) && bytes.Equal(run[cursors[i]].Key, minKey) {
				values = append(values, run[cursors[i]].Values...)
				cursors[i]++
			}
		}
		if err := visit(minKey, values); err != nil {
			return err
		}
	}
}
