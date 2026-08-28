// Package mr has a simple MapReduce implementation, one that does everything
// inside the task manager (no outside files). This limits what it is good for,
// but makes for a lovely stress test, and shows off some useful task manager
// interaction patterns.
package mr

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
	"golang.org/x/sync/errgroup"
)

const claimDuration = 5 * time.Second

// Fingerprint64 produces a 64-bit unsigned integer from a byte string.
func Fingerprint64(key []byte) uint64 {
	h := fnv.New64()
	h.Write(key)
	return h.Sum64()
}

// ShardForKey produces the shard for a given byte slice and number of shards.
func ShardForKey(key []byte, n int) int {
	return int(Fingerprint64(key) % uint64(n))
}

// docRef identifies a doc by namespace and key.
type docRef struct {
	NS  string `json:"ns"`
	Key string `json:"key"`
}

func (r docRef) asClaim() *entroq.DocClaim { return entroq.ClaimKey(r.NS, r.Key) }
func (r docRef) asQuery() *entroq.DocQuery { return &entroq.DocQuery{Namespace: r.NS, KeyExact: r.Key} }

func shardDocKey(n int) string       { return fmt.Sprintf("shard/%d", n) }
func reduceDocKey(key []byte) string { return fmt.Sprintf("reduce/%x", key) }
func resultDocKey(key []byte) string { return fmt.Sprintf("result/%x", key) }

// keyValues is a mapper output entry: one map key with all its emitted values.
type keyValues struct {
	Key    []byte   `json:"key"`
	Values [][]byte `json:"values"`
}

// mapOutput is the value of a map-result task. It points back to the shard doc
// so the controller can delete it, and carries the mapper's grouped key/value
// entries for the shuffle step.
type mapOutput struct {
	Shard   docRef       `json:"shard"`
	Entries []*keyValues `json:"entries"`
}

// reduceOutput is the value of a reduce-result task. Doc is the reduce doc
// reference plumbed through from the reduce task so the controller can claim
// and delete intermediate docs. MapKey is the original map output key, used to
// name the result doc. Result is the final reduced value.
type reduceOutput struct {
	Doc    docRef `json:"doc"`
	MapKey []byte `json:"key"`
	Result []byte `json:"result"`
}

// reduceClaim is the value of a reduce task. It identifies the reduce docs by
// namespace and primary key, and carries the original map output key so the
// result doc can be named correctly.
type reduceClaim struct {
	Doc    docRef `json:"doc"`
	MapKey []byte `json:"map_key"`
}

// Controller coordinates the MapReduce pipeline. It creates shard docs and map
// tasks during Setup, then drives the map and reduce phases by processing
// result tasks from mapper and reducer workers.
//
// Queue layout (all relative to the prefix passed to NewController):
//
//	{prefix}/map           — map input tasks (controller → mappers)
//	{prefix}/map/result    — map result tasks (mappers → controller)
//	{prefix}/reduce        — reduce input tasks (controller → reducers)
//	{prefix}/reduce/result — reduce result tasks (reducers → controller)
//
// All shard, intermediate, and result docs share the namespace equal to prefix.
type Controller struct {
	client *entroq.EntroQ
	prefix string
}

// NewController creates a Controller for the given EntroQ client and prefix.
func NewController(eq *entroq.EntroQ, prefix string) *Controller {
	return &Controller{client: eq, prefix: prefix}
}

// MapQ is the queue that mappers watch for input tasks.
func (c *Controller) MapQ() string { return c.prefix + "/map" }

// MapResultQ is the queue where mappers post their result tasks.
func (c *Controller) MapResultQ() string { return c.prefix + "/map/result" }

// ReduceQ is the queue that reducers watch for input tasks.
func (c *Controller) ReduceQ() string { return c.prefix + "/reduce" }

// ReduceResultQ is the queue where reducers post their result tasks.
func (c *Controller) ReduceResultQ() string { return c.prefix + "/reduce/result" }

// DocNS is the namespace used for all shard, intermediate, and result docs.
func (c *Controller) DocNS() string { return c.prefix }

// Setup creates one shard doc and one map task per input KV. Call this before
// starting mapper workers.
func (c *Controller) Setup(ctx context.Context, input []*KV) error {
	const batchSize = 250
	for start := 0; start < len(input); start += batchSize {
		end := min(start+batchSize, len(input))
		args := make([]entroq.ModifyArg, 0, 2*(end-start))
		for i, kv := range input[start:end] {
			key := shardDocKey(start + i)
			args = append(args,
				entroq.PuttingDocInto(c.DocNS(), entroq.WithKeys(key, ""), entroq.WithContent(kv)),
				entroq.InsertingInto(c.MapQ(), entroq.WithValue(docRef{NS: c.DocNS(), Key: key})),
			)
		}
		if _, err := c.client.Modify(ctx, args...); err != nil {
			return fmt.Errorf("setup shards %d-%d: %w", start, end-1, err)
		}
	}
	return nil
}

// RunMapPhase processes map-result tasks until all shard docs are gone from the
// namespace. It runs a Worker watching MapResultQ alongside a watcher goroutine
// that cancels the phase when no shard docs remain.
//
// The Worker uses TakeDocs to claim each shard doc atomically before DoModify
// runs. When the shard doc is absent (already processed by a speculative
// duplicate mapper), DoModify receives empty docs and simply deletes the task.
// When the doc is contended, the Worker retries automatically. On success it
// atomically deletes the map-result task, deletes the shard doc, and creates
// one reduce doc per output entry.
func (c *Controller) RunMapPhase(ctx context.Context) error {
	phaseCtx, phaseCancel := context.WithCancel(ctx)
	defer phaseCancel()

	g, gctx := errgroup.WithContext(phaseCtx)

	g.Go(func() error {
		return worker.New[mapOutput](c.client,
			worker.WithTakeDocs(func(_ context.Context, _ *entroq.Task, out mapOutput) ([]*entroq.DocClaim, error) {
				return []*entroq.DocClaim{out.Shard.asClaim()}, nil
			}),
			worker.WithDoModify(func(_ context.Context, task *entroq.Task, out mapOutput, docs []*entroq.Doc) (*worker.Result, error) {
				if len(docs) == 0 {
					// Shard doc already gone — duplicate result task; discard it.
					return worker.Modify(task.Delete()), nil
				}
				// Secondary key is a fingerprint of the shard key:
				// unique per shard, deterministic, no shared state needed.
				secondary := fmt.Sprintf("%016x", Fingerprint64([]byte(out.Shard.Key)))
				modArgs := []entroq.ModifyArg{task.Delete(), docs[0].Delete()}
				for _, entry := range out.Entries {
					modArgs = append(modArgs, entroq.PuttingDocInto(c.DocNS(),
						entroq.WithKeys(reduceDocKey(entry.Key), secondary),
						entroq.WithContent(entry),
					))
				}
				return worker.Modify(modArgs...), nil
			}),
		).Run(gctx, worker.Watching(c.MapResultQ()), worker.WithLease(claimDuration))
	})

	// Watcher: cancel the phase when no shard docs remain.
	// Bounds: '/' = 0x2F, '0' = 0x30, so "shard/..." sorts before "shard0".
	g.Go(func() error {
		for {
			shards, err := c.client.Docs(gctx, &entroq.DocQuery{
				Namespace:  c.DocNS(),
				KeyStart:   "shard/",
				KeyEnd:     "shard0",
				OmitValues: true,
				Limit:      1,
			})
			if err != nil {
				if entroq.IsCanceled(err) {
					return nil
				}
				return fmt.Errorf("shard check: %w", err)
			}
			if len(shards) == 0 {
				phaseCancel()
				return nil
			}
			select {
			case <-gctx.Done():
				return nil
			case <-time.After(time.Second):
			}
		}
	})

	return g.Wait()
}

// RunReducePhase discovers all reduce docs created by the map phase, creates
// one reduce task per unique primary key, then processes reduce-result tasks
// until all reduce docs are deleted.
func (c *Controller) RunReducePhase(ctx context.Context) error {
	// Discover all reduce docs and create one reduce task per unique primary key.
	docs, err := c.client.Docs(ctx, &entroq.DocQuery{
		Namespace:  c.DocNS(),
		KeyStart:   "reduce/",
		KeyEnd:     "reduce0",
		OmitValues: true,
	})
	if err != nil {
		return fmt.Errorf("discover reduce docs: %w", err)
	}

	seen := make(map[string]bool)
	for _, d := range docs {
		if seen[d.Key] {
			continue
		}
		seen[d.Key] = true
		// Extract the original map output key from "reduce/<hex>".
		mapKey, err := hex.DecodeString(strings.TrimPrefix(d.Key, "reduce/"))
		if err != nil {
			return fmt.Errorf("decode map key from %q: %w", d.Key, err)
		}
		if _, err := c.client.Modify(ctx,
			entroq.InsertingInto(c.ReduceQ(), entroq.WithValue(reduceClaim{
				Doc:    docRef{NS: c.DocNS(), Key: d.Key},
				MapKey: mapKey,
			})),
		); err != nil {
			return fmt.Errorf("create reduce task for %q: %w", d.Key, err)
		}
	}

	phaseCtx, phaseCancel := context.WithCancel(ctx)
	defer phaseCancel()

	g, gctx := errgroup.WithContext(phaseCtx)

	// Worker processes reduce-result tasks: claims reduce docs, creates result
	// doc, deletes task.
	g.Go(func() error {
		return worker.New[reduceOutput](c.client,
			worker.WithTakeDocs(func(_ context.Context, _ *entroq.Task, out reduceOutput) ([]*entroq.DocClaim, error) {
				return []*entroq.DocClaim{out.Doc.asClaim()}, nil
			}),
			worker.WithDoModify(func(_ context.Context, task *entroq.Task, out reduceOutput, docs []*entroq.Doc) (*worker.Result, error) {
				if len(docs) == 0 {
					// Reduce docs already gone — duplicate result task; discard.
					return worker.Modify(task.Delete()), nil
				}
				modArgs := []entroq.ModifyArg{task.Delete()}
				for _, d := range docs {
					modArgs = append(modArgs, d.Delete())
				}
				modArgs = append(modArgs, entroq.PuttingDocInto(c.DocNS(),
					entroq.WithKeys(resultDocKey(out.MapKey), ""),
					entroq.WithContent(&keyValues{Key: out.MapKey, Values: [][]byte{out.Result}}),
				))
				return worker.Modify(modArgs...), nil
			}),
		).Run(gctx, worker.Watching(c.ReduceResultQ()), worker.WithLease(claimDuration))
	})

	// Watcher: cancel the phase when no reduce docs remain.
	g.Go(func() error {
		for {
			reduceDocs, err := c.client.Docs(gctx, &entroq.DocQuery{
				Namespace:  c.DocNS(),
				KeyStart:   "reduce/",
				KeyEnd:     "reduce0",
				OmitValues: true,
				Limit:      1,
			})
			if err != nil {
				if entroq.IsCanceled(err) {
					return nil
				}
				return fmt.Errorf("reduce doc check: %w", err)
			}
			if len(reduceDocs) == 0 {
				phaseCancel()
				return nil
			}
			select {
			case <-gctx.Done():
				return nil
			case <-time.After(time.Second):
			}
		}
	})

	return g.Wait()
}

// sliceReducerInput implements ReducerInput over a pre-sorted [][]byte slice.
type sliceReducerInput struct {
	key    []byte
	values [][]byte
	idx    int
}

func (s *sliceReducerInput) Key() []byte { return s.key }
func (s *sliceReducerInput) Err() error  { return nil }
func (s *sliceReducerInput) Value() []byte {
	if s.idx == 0 || s.idx > len(s.values) {
		return nil
	}
	return s.values[s.idx-1]
}
func (s *sliceReducerInput) Next() bool {
	if s.idx >= len(s.values) {
		return false
	}
	s.idx++
	return true
}

// MapperWorker creates a worker that watches MapQ, reads the shard doc, runs
// mapFn over its key/value, groups emitted output by key (sorting values
// within each key), and posts a mapOutput to MapResultQ.
func (c *Controller) MapperWorker(mapFn Mapper) *worker.Worker[docRef] {
	return worker.New[docRef](c.client,
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, ref docRef, _ []*entroq.Doc) (*worker.Result, error) {
			shardDocs, err := c.client.Docs(ctx, ref.asQuery())
			if err != nil {
				return nil, fmt.Errorf("mapper read shard: %w", err)
			}
			if len(shardDocs) == 0 {
				// Shard already gone — duplicate task; discard.
				return worker.Modify(task.Delete()), nil
			}
			var kv KV
			if err := json.Unmarshal(shardDocs[0].Content, &kv); err != nil {
				return nil, fmt.Errorf("mapper parse shard: %w", err)
			}

			// Collect emitted key/values, grouping by key.
			kvMap := make(map[string][][]byte)
			var keyOrder []string
			emit := func(_ context.Context, k, v []byte) error {
				ks := string(k)
				if _, exists := kvMap[ks]; !exists {
					keyOrder = append(keyOrder, ks)
				}
				kvMap[ks] = append(kvMap[ks], v)
				return nil
			}
			if err := mapFn(ctx, kv.Key, kv.Value, emit); err != nil {
				return nil, fmt.Errorf("mapper run: %w", err)
			}

			// Sort keys lexicographically; sort values within each key.
			sort.Strings(keyOrder)
			entries := make([]*keyValues, 0, len(keyOrder))
			for _, ks := range keyOrder {
				vals := kvMap[ks]
				sort.Slice(vals, func(i, j int) bool {
					return bytes.Compare(vals[i], vals[j]) < 0
				})
				entries = append(entries, &keyValues{Key: []byte(ks), Values: vals})
			}

			return worker.Modify(
				task.Delete(),
				entroq.InsertingInto(c.MapResultQ(), entroq.WithValue(mapOutput{
					Shard:   ref,
					Entries: entries,
				})),
			), nil
		}),
	)
}

// ReducerWorker creates a worker that watches ReduceQ, reads all reduce docs
// for the claimed primary key, merges and sorts their values, runs reduceFn,
// and posts a reduceOutput to ReduceResultQ.
func (c *Controller) ReducerWorker(reduceFn Reducer) *worker.Worker[reduceClaim] {
	return worker.New[reduceClaim](c.client,
		worker.WithDoModify(func(ctx context.Context, task *entroq.Task, rc reduceClaim, _ []*entroq.Doc) (*worker.Result, error) {
			docs, err := c.client.Docs(ctx, rc.Doc.asQuery())
			if err != nil {
				return nil, fmt.Errorf("reducer read docs %q: %w", rc.Doc.Key, err)
			}

			// Merge values from all reduce docs for this primary key.
			var values [][]byte
			for _, d := range docs {
				var kv keyValues
				if err := json.Unmarshal(d.Content, &kv); err != nil {
					return nil, fmt.Errorf("reducer parse doc %q: %w", d.ID, err)
				}
				values = append(values, kv.Values...)
			}
			sort.Slice(values, func(i, j int) bool {
				return bytes.Compare(values[i], values[j]) < 0
			})

			result, err := reduceFn(ctx, &sliceReducerInput{key: rc.MapKey, values: values})
			if err != nil {
				return nil, fmt.Errorf("reducer compute: %w", err)
			}

			return worker.Modify(
				task.Delete(),
				entroq.InsertingInto(c.ReduceResultQ(), entroq.WithValue(reduceOutput{
					Doc:    rc.Doc,
					MapKey: rc.MapKey,
					Result: result,
				})),
			), nil
		}),
	)
}

// RunAll runs the full MapReduce pipeline: setup, map phase with numMappers
// concurrent mapper workers, then reduce phase with numReducers concurrent
// reducer workers. On success, result docs are available in DocNS under keys
// matching "result/<hex(mapOutputKey)>".
func RunAll(ctx context.Context, eq *entroq.EntroQ, prefix string, input []*KV, mapFn Mapper, reduceFn Reducer, numMappers, numReducers int) error {
	ctrl := NewController(eq, prefix)
	if err := ctrl.Setup(ctx, input); err != nil {
		return fmt.Errorf("setup: %w", err)
	}

	// Map phase: mapper workers + controller phase runner.
	mapPhaseCtx, mapPhaseCancel := context.WithCancel(ctx)
	defer mapPhaseCancel()

	mg, mgctx := errgroup.WithContext(mapPhaseCtx)

	for range numMappers {
		mg.Go(func() error {
			return ctrl.MapperWorker(mapFn).Run(mgctx,
				worker.Watching(ctrl.MapQ()),
				worker.WithLease(claimDuration),
			)
		})
	}
	mg.Go(func() error {
		err := ctrl.RunMapPhase(mgctx)
		mapPhaseCancel() // stop mappers once phase completes
		return err
	})

	if err := mg.Wait(); err != nil {
		return fmt.Errorf("map phase: %w", err)
	}

	// Reduce phase: reducer workers + controller phase runner.
	reducePhaseCtx, reducePhaseCancel := context.WithCancel(ctx)
	defer reducePhaseCancel()

	rg, rgctx := errgroup.WithContext(reducePhaseCtx)

	for range numReducers {
		rg.Go(func() error {
			return ctrl.ReducerWorker(reduceFn).Run(rgctx,
				worker.Watching(ctrl.ReduceQ()),
				worker.WithLease(claimDuration),
			)
		})
	}
	rg.Go(func() error {
		err := ctrl.RunReducePhase(rgctx)
		reducePhaseCancel() // stop reducers once phase completes
		return err
	})

	return rg.Wait()
}

// Results returns all result key/value pairs from a completed RunAll, sorted
// by key. ns must match the prefix passed to RunAll or NewController.DocNS.
func Results(ctx context.Context, eq *entroq.EntroQ, ns string) ([]*KV, error) {
	docs, err := eq.Docs(ctx, &entroq.DocQuery{
		Namespace: ns,
		KeyStart:  "result/",
		KeyEnd:    "result0",
	})
	if err != nil {
		return nil, fmt.Errorf("results: %w", err)
	}
	var kvs []*KV
	for _, d := range docs {
		var kv keyValues
		if err := json.Unmarshal(d.Content, &kv); err != nil {
			return nil, fmt.Errorf("result parse %q: %w", d.Key, err)
		}
		var val []byte
		if len(kv.Values) > 0 {
			val = kv.Values[0]
		}
		kvs = append(kvs, NewKV(kv.Key, val))
	}
	return kvs, nil
}

// MapEmitFunc is the emit function passed to mappers.
type MapEmitFunc func(ctx context.Context, key, value []byte) error

// Mapper is called once per input KV. It emits zero or more key/value pairs
// for the reduce phase by calling emit.
type Mapper func(ctx context.Context, key, value []byte, emit MapEmitFunc) error

// IdentityMapper produces the same output as its input.
func IdentityMapper(ctx context.Context, key, value []byte, emit MapEmitFunc) error {
	return emit(ctx, key, value)
}

// WordCountMapper produces word:1 for each word in the value. The input key is
// ignored. Splitting is purely based on whitespace, and is quite naive.
func WordCountMapper(ctx context.Context, key, value []byte, emit MapEmitFunc) error {
	words := make(map[string]int)
	for w := range strings.FieldsSeq(string(value)) {
		words[w]++
	}
	numEmitted := 0
	for word, count := range words {
		if (numEmitted+1)%1000 == 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("canceled map operation: %w", ctx.Err())
			default:
			}
		}
		if err := emit(ctx, []byte(word), []byte(fmt.Sprint(count))); err != nil {
			return fmt.Errorf("word count output error: %w", err)
		}
		numEmitted++
	}
	return nil
}

// KV contains a key/value pair. It is the input type for mappers and is stored
// as the content of shard docs.
type KV struct {
	Key   []byte `json:"key"`
	Value []byte `json:"value"`
}

// NewKV creates a new key/value struct.
func NewKV(key, value []byte) *KV {
	return &KV{Key: key, Value: value}
}

// String converts this key/value pair into a readable string.
func (kv *KV) String() string {
	return fmt.Sprintf("(%s)=%s", string(kv.Key), string(kv.Value))
}

// ReducerInput provides a streaming interface for values during reduction.
type ReducerInput interface {
	// Key is always available; it is the map output key being reduced.
	Key() []byte
	// Value is the current value. Call Next before the first call.
	Value() []byte
	// Err returns any iteration error. Check after Next returns false.
	Err() error
	// Next advances to the next value. Returns false when exhausted.
	//
	//	for input.Next() {
	//		process(input.Value())
	//	}
	//	if err := input.Err(); err != nil { ... }
	Next() bool
}

// Reducer is called once per unique map-output key. It receives all values for
// that key via input and must return a single combined value.
type Reducer func(ctx context.Context, input ReducerInput) ([]byte, error)

// FirstValueReducer outputs its first value and quits.
func FirstValueReducer(_ context.Context, input ReducerInput) ([]byte, error) {
	if !input.Next() {
		return nil, fmt.Errorf("no inputs to reducer")
	}
	if err := input.Err(); err != nil {
		return nil, fmt.Errorf("reduce: %w", err)
	}
	return input.Value(), nil
}

// NilReducer produces a single nil value for the provided key.
func NilReducer(_ context.Context, _ ReducerInput) ([]byte, error) {
	return nil, nil
}

// SumReducer produces a sum over integer values for each key.
func SumReducer(_ context.Context, input ReducerInput) ([]byte, error) {
	sum := 0
	for input.Next() {
		count, err := strconv.Atoi(string(input.Value()))
		if err != nil {
			return nil, fmt.Errorf("int conversion in SumReducer: %w", err)
		}
		sum += count
	}
	if err := input.Err(); err != nil {
		return nil, fmt.Errorf("get SumReducer value: %w", err)
	}
	return []byte(fmt.Sprint(sum)), nil
}

// SliceReducer produces a JSON-serialized slice of all values in its input.
func SliceReducer(_ context.Context, input ReducerInput) ([]byte, error) {
	var vals [][]byte
	for input.Next() {
		vals = append(vals, input.Value())
	}
	if err := input.Err(); err != nil {
		return nil, fmt.Errorf("get reduce value: %w", err)
	}
	return json.Marshal(vals)
}
