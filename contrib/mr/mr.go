// Package mr has a simple MapReduce implementation, one that does everything
// inside the task manager (no outside files). This limits what it is good for,
// but makes for a lovely stress test, and shows off some useful task manager
// interaction patterns.
package mr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shiblon/entroq"
)

const (
	taskRenewalInterval = 30 * time.Second
	taskRenewalDuration = 2 * taskRenewalInterval
)

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

// MapEmitFunc is the emit function passed to mappers.
type MapEmitFunc func(ctx context.Context, key, value []byte) error

// MapEmitter is passed to a map input processor so it can emit multiple
// outputs for a single input by calling it.
type MapEmitter interface {
	// Emit is called to output data from a mapper.
	Emit(ctx context.Context, key, value []byte) error

	// AsModifyArgs takes a *completed* map output and produces shuffle task
	// insertions from that output. Adds optional additional arguments as
	// needed by the caller (for example, the caller may desire to delete the
	// map task at the same time). Tasks are added to the queue <prefix>/<shard>.
	AsModifyArgs(prefix string, additional ...entroq.ModifyArg) ([]entroq.ModifyArg, error)
}

// CollectingMapEmitter collects all of its output into a slice of shards, each
// member of which contains a slice of kev/value pairs.
type CollectingMapEmitter struct {
	NumShards int
	shards    [][]*KV
}

// NewCollectingMapEmitter creates a shard map emitter for use by a mapper.
// When mapping is done, the data is collected into sorted slices of key/value
// pairs, one per shard.
func NewCollectingMapEmitter(numShards int) *CollectingMapEmitter {
	if numShards < 1 {
		numShards = 1
	}

	return &CollectingMapEmitter{
		NumShards: numShards,
		shards:    make([]*KV, numShards),
	}
}

// Emit adds a new key/value pair to the emitter.
func (e *CollectingMapEmitter) Emit(_ context.Context, key, value []byte) error {
	shard := ShardForKey(key)
	e.shards[shard] = append(e.shards[shard], NewKV(key, value))
}

// AsModifyArgs returns a slice of arguments to be sent to insert new shuffle
// tasks after emissions are complete. Additional modifications can be passed in
// to make, e.g., simultaneous task deletion easier to specify.
func (e *CollectingMapEmitter) AsModifyArgs(qPrefix string, additional ...entroq.ModifyArg) ([]entroq.ModifyArg, error) {
	var args []entroq.ModifyArg
	for shard, kvs := range e.shards {
		if len(kvs) == 0 {
			continue
		}
		sort.Slice(kvs, func(i, j int) bool {
			return bytes.Compare(kvs[i].Key, kvs[j].Key) < 0
		})
		value, err := json.Marshal(kvs)
		if err != nil {
			return nil, fmt.Errorf("emit to modify args: %v", err)
		}
		queue := path.Join(qPrefix, fmt.Sprint(shard))
		args = append(args, entroq.InsertingInto(queue, entroq.WithValue(value)))
	}
	return append(args, additional...), nil
}

// MapProcessor is a function that accepts a key/value pair and emits zero or
// more key/value pairs for reducing.
type Mapper func(ctx context.Context, key, value []byte, emit MapEmitFunc) error

// IdentityMapper produces the same output as its input.
func IdentityMapper(_ context.Context, key, value []byte, emit MapEmitFunc) error {
	return emit(key, value)
}

// WordCountMapper produces word:1 for each word in the value. The input key is
// ignored. Splitting is purely based on whitespace, and is quite naive.
func WordCountMapper(ctx context.Context, key, value []byte, emit MapEmitFunc) error {
	words := make(map[string]int)
	for _, w := range strings.Fields(string(value)) {
		words[w]++
	}
	numEmitted := 0
	for word, count := range words {
		// Pause every so often to check for cancelation.
		if (numEmitted+1)%1000 == 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("canceled map operation")
			default:
			}
		}
		if err := emit([]byte(word), []byte(fmt.Sprint(count))); err != nil {
			return fmt.Errorf("word count output error: %v", err)
		}
		numEmitted++
	}
	return nil
}

// KV contains instructions for a mapper. It is just a key and value.
type KV struct {
	Key   []byte `json:"key"`
	Value []byte `json:"value"`
}

// NewKV creates a new key/value struct.
func NewKV(key, value []byte) *KV {
	return &KV{Key: key, Value: value}
}

// MapWorker claims map input tasks, processes them, and produces output tasks.
type MapWorker struct {
	client     *entroq.EntroQ
	newEmitter func() MapEmitter

	ClaimantID uuid.UUID

	InputQueue   string
	OutputPrefix string
	Map          Mapper
}

// MapWorkerOption is passed to NewMapWorker to change what it does.
type MapWorkerOption func(*MapWorker)

// WithMapper provides a mapper process to a map worker. The default is
// IdentityMapper if not specified as an option.
func WithMapper(m Mapper) MapWorkerOption {
	return func(mw *MapWorker) {
		if m == nil {
			return
		}
		mw.Map = m
	}
}

// MapToOutputPrefix provides an output queue prefix separate from the input
// queue for mappers to place output tasks into. If not provided, mappers
// append "done" to the input queue to form the output queue prefix.
func MapToOutputPrefix(p string) MapWorkerOption {
	return func(mw *MapWorker) {
		if p == "" {
			return
		}
		mw.OutputPrefix = p
	}
}

// NewMapWorker creates a new MapWorker, which loops until told to stop,
// claiming tasks and processing them, placing them into an appropriate output
// queue calculated from the output key.
func NewMapWorker(eq *entroq.EntroQ, inQueue string, newEmitter func() MapEmitter, opts ...MapWorkerOption) *MapWorker {
	w := &MapWorker{
		client:       eq,
		newEmitter:   newEmitter,
		ClaimantID:   uuid.New(),
		InputQueue:   inQueue,
		OutputPrefix: inQueue,
		Map:          IdentityMapper,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// mapTask runs a mapper over a given task's input. It does everything in
// memory, including storage of outputs. Note that, were we to want this to
// scale, we would need to write output to a number of temporary files, then
// clean them up if there were an error of any kind (or let them get garbage
// collected). They would need to not be referenced unless the entire map task
// were completed, at which point they would be pointed to by the completion
// task. That's very doable, but beyond the scope of this exercise.
func (w *MapWorker) mapTask(ctx context.Context, task *entroq.Task) error {
	emitter := w.newEmitter()
	finalTask, err := w.client.DoWithRenewal(ctx, task, taskRenewalDuration,
		func(ctx context.Context, id uuid.UUID, data *entroq.TaskData) error {
			kv := new(KV)
			if err := json.Unmarshal(data.Value, kv); err != nil {
				return fmt.Errorf("map run json unmarshal: %v", err)
			}

			if err := w.Map(ctx, kv.Key, kv.Value, emitter.Emit); err != nil {
				return fmt.Errorf("map task %s failed: %v", id, err)
			}

			return nil
		})
	if err != nil {
		return fmt.Errorf("claim task for map: %v", err)
	}

	// Delete map task and create shuffle shard tasks.
	args := emitter.AsModifyArgs(w.OutputPrefix, finalTask.AsDeletion())
	if _, _, err := w.client.Modify(ctx, w.ClaimantID, modifyArgs...); err != nil {
		return fmt.Errorf("map task update failed: %v", err)
	}
	return nil
}

// Run runs the map worker. It blocks, running until the map queue is
// empty, it encounters an error, or its context is canceled, whichever comes
// first. If this should be run in a goroutine, that is up to the caller.
// The task value is expected to be a JSON-serialized KV struct.
func (w *MapWorker) Run(ctx context.Context) error {
	for {
		empty, err := w.client.QueuesEmpty(ctx, entroq.MatchExact(w.InputQueue))
		if err != nil {
			return fmt.Errorf("map test empty: %v", err)
		}
		if empty {
			return nil // all finished.
		}
		task, err := w.client.Claim(ctx, w.ClaimantID, w.InputQueue, taskRenewalDuration)
		if err != nil {
			return fmt.Errorf("map run claim: %v", err)
		}
		if err := w.mapTask(ctx, task); err != nil {
			return fmt.Errorf("map run: %v", err)
		}
	}
}

// ReducerInput provides a streaming interface for getting values during reduction.
type ReducerInput interface {
	// Key produces the key for this reduce operation.
	Key() []byte

	// Value outputs the current value in the input.
	Value() []byte

	// Err returns any errors encountered while iterating over input.
	Err() error

	// Next must be called before Value() (but Key() is always available).
	//
	// Example:
	//
	// 	for input.Next() {
	// 		process(input.Value())
	// 	}
	// 	if err := input.Err(); err != nil {
	// 		return fmt.Errorf("error getting input: %v", err)
	// 	}
	Next() bool
}

// Reducer is called once per unique map-output key. It is expected to output a
// single value for all inputs.
type Reducer func(ctx context.Context, input ReducerInput) ([]byte, error)

// FirstValueReducer outputs its first value and quits.
func FirstValueReducer(ctx context.Context, input ReducerInput) ([]byte, error) {
	if !input.Next() {
		return nil, fmt.Errorf("no inputs to reducer")
	}
	if err := input.Err(); err != nil {
		return nil, fmt.Errorf("reduce error: %v", err)
	}
	return input.Value(), nil
}

// NilReducer produces a single nil value for the provided key. This can be useful for
// sorting keys, for example, where the values are not useful or important.
func NilReducer(ctx context.Context, input ReducerInput) ([]byte, error) {
	return nil, nil
}

// SliceReducer produces a JSON-serialized slice of all values in its input.
func SliceReducer(ctx context.Context, input ReducerInput) ([]byte, error) {
	var vals [][]byte
	for input.Next() {
		vals = append(vals, input.Value())
	}
	if err := input.Err(); err != nil {
		return nil, fmt.Errorf("error getting reduce value: %v", err)
	}
	return json.Marshal(vals)
}

// ReduceWorker consumes shuffle output and combines all values for a
// particular key into a single key/value pair, which is then JSON-serialized,
// one item per line, and written to a provided emitter.
type ReduceWorker struct {
	client *entroq.EntroQ

	ClaimantID uuid.UUID

	MapEmptyQueue string
	InputQueue    string
	OutputQueue   string
	Reduce        Reducer
}

// ReduceWorkerOption is passed to NewReduceWorker to specify non-default options.
type ReduceWorkerOption func(*ReduceWorker)

// WithReducer specifies the reducer, otherwise NilReducer is used.
func WithReducer(reduce Reducer) ReduceWorkerOption {
	return func(w *ReduceWorker) {
		w.Reduce = reduce
	}
}

// ReduceToOutput specifies the output queue name for finished reduce shards.
func ReduceToOutput(q string) ReduceWorkerOption {
	return func(w *ReduceWorker) {
		w.OutputQueue = q
	}
}

// NewReduceWorker creates a reduce worker for the given task client and input
// queue, running the reducer over every unique key.
func NewReduceWorker(eq *entroq.EntroQ, mapEmptyQueue, inQueue string, opts ...ReduceWorkerOption) *ReduceWorker {
	w := &ReduceWorker{
		client:        eq,
		ClaimantID:    uuid.New(),
		InputQueue:    inQueue,
		MapEmptyQueue: mapEmptyQueue,
		OutputQueue:   path.Join(inQueue, "out"),
		Reduce:        NilReducer,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

type proxyingReduceInput struct {
	Key   func() []byte
	Next  func() bool
	Value func() []byte
	Err   func() error
}

func (p *proxyingReduceInput) Key() []byte   { return p.Key() }
func (p *proxyingReduceInput) Next() bool    { return p.Next() }
func (p *proxyingReduceInput) Value() []byte { return p.Value() }
func (p *proxyingReduceInput) Err() error    { return p.Err() }

// mergeTasks takes multiple tasks as input, merges them together, and replaces them with a single task.
func (w *ReduceWorker) mergeTasks(ctx context.Context, tasks []*entroq.Task) error {
	if len(tasks) <= 1 {
		return nil
	}
	var modArgs []entroq.ModifyArg
	finalTasks, err := w.client.DoWithRenewAll(ctx, tasks, taskRenewalDuration,
		func(ctx context.Context, ids []uuid.UUID, dats []*entroq.TaskData) error {
			// Do a merge sort (linear, already sorted inputs).
			allKVs := make([]*KV, len(tasks))
			indices := make([]int, len(tasks))

			for _, task := range tasks {
				var kvs []*KV
				if err := json.Unmarshal(task.Value, &kvs); err != nil {
					return fmt.Errorf("unmarshal merge data: %v", err)
				}
				allKVs = append(allKVs, kvs)
				indices = append(indices, 0)
				modArgs = append(modArgs, task.AsDeletion())
			}

			var vals []*KV

			for {
				best := -1
				for i, kvs := range allKVs {
					// Skip any that are exhausted.
					top := indices[i]
					if top >= len(kvs) {
						continue
					}
					if best < 0 || bytes.Compare(kvs[top].Key, allKVs[best][indices[best]].Key) < 0 {
						best = i
					}
				}
				if best < 0 {
					break
				}
				vals = append(vals, allKVs[best][indices[best]])
				indices[best]++
			}

			// Now all key/value pairs are merged into a single sorted list. Create a new task and delete the others.
			combined, err := json.Marshal(vals)
			if err != nil {
				return fmt.Errorf("marshal combined merge: %v", err)
			}

			modArgs = append(modArgs, entroq.InsertingInto(w.OutputQueue, entroq.WithValue(combined)))
			return nil
		})
	if err != nil {
		return fmt.Errorf("claim tasks for merge: %v", err)
	}
	if _, _, err := w.client.Modify(ctx, w.ClaimantID, modArgs...); err != nil {
		return fmt.Errorf("merge output: %v", err)
	}
	return nil
}

// reduceTask takes a task (which is a list of key/value pairs, sorted),
// and runs a reduce function on each set corresponding to a unique key,
// writing them out in the order they appear (to maintain sorting).
func (w *ReduceWorker) reduceTask(ctx context.Context, task *entroq.Task) error {
	var outputs []*KV
	finalTask, err := w.client.DoWithRenewal(ctx, task, taskRenewalDuration,
		func(ctx context.Context, id uuid.UUID, data *entroq.TaskData) error {
			kvs := make([]*KV)
			if err := json.Unmarshal(data.Value, &kvs); err != nil {
				return fmt.Errorf("json unmarshal reduce pairs: %v", err)
			}

			if len(kvs) == 0 {
				return nil
			}

			last := new(KV)
			curr := 0

			for next < len(kvs) {
				input := &proxyingReduceInput{
					Key:   func() []byte { return last.Key },
					Value: func() []byte { return last.Value },
					Err:   func() []byte { return nil },
					Next: func() bool {
						if curr >= len(kvs) {
							return false
						}
						if curr == 0 || bytes.Compare(last.Key, kvs[curr].Key) == 0 {
							last = kvs[curr]
							curr++
							return true
						}
						return false
					},
				}
				output, err := w.Reduce(ctx, input)
				if err != nil {
					return fmt.Errorf("reduce error: %v", err)
				}
				outputs = append(outputs, NewKV(curr.Key, output))
			}
			return nil
		})

	outputValue, err := json.Marshal(outputs)
	if err != nil {
		return fmt.Errorf("failed to serialize reduce outputs: %v", err)
	}
	if _, _, err := w.client.Modify(ctx, w.ClaimantID, finalTask.AsDeletion(), entroq.InsertingInto(w.OutputQueue, entroq.WithValue(outputValue))); err != nil {
		return fmt.Errorf("failed to insert completed reduce shard: %v", err)
	}
	return nil
}

// Run starts a worker that consumes all reduce tasks in a particular queue,
// and quits when it is empty. It actually does map output merging, too, since
// there are exactly as many mergers as reducers, and reduce cannot proceed
// until merging is finished.
//
// The worker watches for
// - more than one task in its input queue, and
// - an empty map queue.
//
// This worker has no need to claim tasks to do shuffling. It is assigned a specific
// queue representing its shard, and no other worker will be assigned the same queue.
//
// Shuffle logic is as follows. In a watch/sleep loop,
// - When more than one task is in the input queue, merge them into a single sorted task and replace.
// - If only one task exists and there are no more map tasks, proceed to reduce.
//
// Reduce logic:
// - pull (singleton) task from input queue
// - run reduce over it and place the resulting sorted key/value pairs into the output queue.
// - quit
func (w *ReduceWorker) Run(ctx context.Context) error {
	// First, merge until there is no more mapping work to do.
	for {
		mergeTasks, err := w.client.Tasks(ctx, w.InputQueue)
		if err != nil {
			return fmt.Errorf("merge task list: %v", err)
		}
		if len(mergeTasks) <= 1 {
			empty, err := w.client.QueuesEmpty(ctx, entroq.MatchExact(w.MapEmptyQueue))
			if err != nil {
				return fmt.Errorf("empty check in merge: %v", err)
			}
			if empty {
				break // all done - no more map tasks, 1 or fewer merge tasks.
			}

			// Nothing to do - sleep and continue.
			select {
			case <-ctx.Done():
				return fmt.Errorf("context canceled while shuffling")
			case <-time.After(5 * time.Second):
			}
			continue
		}
		// More than one merge task is in the queue. Shuffle and check again.
		if err := w.mergeTasks(ctx, mergeTasks); err != nil {
			return fmt.Errorf("merge: %v", err)
		}
	}

	// Then, reduce over the final merged task.
	task, err := w.client.Claim(ctx, w.ClaimantID, w.InputQueue, taskRenewalDuration)
	if err != nil {
		return fmt.Errorf("reduce run claim: %v", err)
	}
	if err := w.reduceTask(ctx, task); err != nil {
		return fmt.Errorf("reduce run: %v", err)
	}
}

// TODO:
// master:
// - read from input stream, getting keys and values
// - push all tasks to map queue
// - start mappers (this makes everything below much simpler - shufflers can just check for empty queue)
// - create shuffle tasks, one per reduce shard, place in shuffle queue
