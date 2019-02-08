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
	"log"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shiblon/entroq"
	"golang.org/x/sync/errgroup"
)

const (
	claimDuration = time.Minute
	claimWait     = 5 * time.Second
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
		shards:    make([][]*KV, numShards),
	}
}

// Emit adds a new key/value pair to the emitter.
func (e *CollectingMapEmitter) Emit(_ context.Context, key, value []byte) error {
	shard := ShardForKey(key, e.NumShards)
	e.shards[shard] = append(e.shards[shard], NewKV(key, value))
	return nil
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
func IdentityMapper(ctx context.Context, key, value []byte, emit MapEmitFunc) error {
	return emit(ctx, key, value)
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
		if err := emit(ctx, []byte(word), []byte(fmt.Sprint(count))); err != nil {
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

// String converts this key/value pair into a readable string.
func (kv *KV) String() string {
	return fmt.Sprintf("%s=%s", string(kv.Key), string(kv.Value))
}

// MapWorker claims map input tasks, processes them, and produces output tasks.
type MapWorker struct {
	client     *entroq.EntroQ
	newEmitter func() MapEmitter

	Name string

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

// MapAsName sets the name for this worker. Worker names are empty by default.
func MapAsName(name string) MapWorkerOption {
	return func(mw *MapWorker) {
		mw.Name = name
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
	task, err := w.client.DoWithRenew(ctx, task, claimDuration, func(ctx context.Context) error {
		kv := new(KV)
		if err := json.Unmarshal(task.Value, kv); err != nil {
			return fmt.Errorf("map run json unmarshal: %v", err)
		}

		if err := w.Map(ctx, kv.Key, kv.Value, emitter.Emit); err != nil {
			return fmt.Errorf("map task %s failed: %v", task.IDVersion(), err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("claim task for map: %v", err)
	}

	// Delete map task and create shuffle shard tasks.
	args, err := emitter.AsModifyArgs(w.OutputPrefix, task.AsDeletion())
	if err != nil {
		return fmt.Errorf("map task make insertions: %v", err)
	}
	if _, _, err := w.client.Modify(ctx, args...); err != nil {
		return fmt.Errorf("map task update failed: %v", err)
	}
	return nil
}

// Run runs the map worker. It blocks, running until the map queue is
// empty, it encounters an error, or its context is canceled, whichever comes
// first. If this should be run in a goroutine, that is up to the caller.
// The task value is expected to be a JSON-serialized KV struct.
func (w *MapWorker) Run(ctx context.Context) error {
	log.Printf("Mapper %q starting", w.Name)
	defer log.Printf("Mapper %q finished", w.Name)
	for {
		empty, err := w.client.QueuesEmpty(ctx, entroq.MatchExact(w.InputQueue))
		if err != nil {
			return fmt.Errorf("map test empty: %v", err)
		}
		if empty {
			return nil // all finished.
		}

		claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		task, err := w.client.Claim(claimCtx, w.InputQueue, claimDuration)
		if entroq.IsTimeout(err) {
			continue
		}
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

// SumReducer produces a sum over (int) values for each key.
func SumReducer(ctx context.Context, input ReducerInput) ([]byte, error) {
	sum := 0
	for input.Next() {
		count, err := strconv.Atoi(string(input.Value()))
		if err != nil {
			return nil, fmt.Errorf("int conversion in SumReducer: %v", err)
		}
		sum += count
	}
	if err := input.Err(); err != nil {
		return nil, fmt.Errorf("error getting SumReducer value: %v", err)
	}
	return []byte(fmt.Sprint(sum)), nil
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

	Name string

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

// ReduceAsName sets the name of this reduce worker, defaults to blank.
func ReduceAsName(name string) ReduceWorkerOption {
	return func(w *ReduceWorker) {
		w.Name = name
	}
}

// NewReduceWorker creates a reduce worker for the given task client and input
// queue, running the reducer over every unique key.
func NewReduceWorker(eq *entroq.EntroQ, mapEmptyQueue, inQueue string, opts ...ReduceWorkerOption) *ReduceWorker {
	w := &ReduceWorker{
		client:        eq,
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
	key   func() []byte
	next  func() bool
	value func() []byte
	err   func() error
}

func (p *proxyingReduceInput) Key() []byte   { return p.key() }
func (p *proxyingReduceInput) Next() bool    { return p.next() }
func (p *proxyingReduceInput) Value() []byte { return p.value() }
func (p *proxyingReduceInput) Err() error    { return p.err() }

// mergeTasks takes multiple tasks as input, merges them together, and replaces them with a single task.
func (w *ReduceWorker) mergeTasks(ctx context.Context, tasks []*entroq.Task) error {
	if len(tasks) <= 1 {
		return nil
	}
	var modArgs []entroq.ModifyArg
	tasks, err := w.client.DoWithRenewAll(ctx, tasks, claimDuration, func(ctx context.Context) error {
		// Append and sort. Note that in real life with real scale, we would
		// want to do an on-disk merge sort (since individual components would
		// already be sorted).
		var kvs []*KV

		for _, task := range tasks {
			var vals []*KV
			if err := json.Unmarshal(task.Value, &vals); err != nil {
				return fmt.Errorf("unmarshal merge data: %v", err)
			}
			kvs = append(kvs, vals...)
		}

		sort.Slice(kvs, func(i, j int) bool {
			return bytes.Compare(kvs[i].Key, kvs[j].Key) < 0
		})

		// Now all key/value pairs are merged into a single sorted list. Create a new task and delete the others.
		combined, err := json.Marshal(kvs)
		if err != nil {
			return fmt.Errorf("marshal combined merge: %v", err)
		}

		modArgs = append(modArgs, entroq.InsertingInto(w.InputQueue, entroq.WithValue(combined)))
		return nil
	})
	if err != nil {
		return fmt.Errorf("claim tasks for merge: %v", err)
	}

	for _, t := range tasks {
		modArgs = append(modArgs, t.AsDeletion())
	}
	if _, _, err := w.client.Modify(ctx, modArgs...); err != nil {
		return fmt.Errorf("merge output: %v", err)
	}
	return nil
}

// reduceTask takes a task (which is a list of key/value pairs, sorted),
// and runs a reduce function on each set corresponding to a unique key,
// writing them out in the order they appear (to maintain sorting).
func (w *ReduceWorker) reduceTask(ctx context.Context, task *entroq.Task) error {
	var outputs []*KV
	task, err := w.client.DoWithRenew(ctx, task, claimDuration, func(ctx context.Context) error {
		log.Printf("Reduce %q starting reduce task", w.Name)
		var kvs []*KV
		if err := json.Unmarshal(task.Value, &kvs); err != nil {
			return fmt.Errorf("json unmarshal reduce pairs: %v", err)
		}

		if len(kvs) == 0 {
			return nil
		}

		curr := 0

		for curr < len(kvs) {
			last := kvs[curr] // starting out - "last" is always first entry

			input := &proxyingReduceInput{
				key:   func() []byte { return last.Key },
				value: func() []byte { return last.Value },
				err:   func() error { return nil },
				next: func() bool {
					if curr >= len(kvs) {
						return false
					}
					// If we aren't just started, and the current key is
					// not the same as the last, can't continue.
					if curr > 0 && bytes.Compare(last.Key, kvs[curr].Key) != 0 {
						return false
					}
					last = kvs[curr]
					curr++
					return true
				},
			}
			currBefore := curr
			output, err := w.Reduce(ctx, input)
			if currBefore == curr {
				// The reducer didn't consume anything - we need to do that instead.
				for input.Next() {
				}
			}
			if err != nil {
				return fmt.Errorf("reduce error: %v", err)
			}
			outputs = append(outputs, NewKV(last.Key, output))
		}
		return nil
	})

	outputValue, err := json.Marshal(outputs)
	if err != nil {
		return fmt.Errorf("failed to serialize reduce outputs: %v", err)
	}
	if _, _, err := w.client.Modify(ctx, task.AsDeletion(), entroq.InsertingInto(w.OutputQueue, entroq.WithValue(outputValue))); err != nil {
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
	log.Printf("Reducer %q starting on queue %q", w.Name, w.InputQueue)
	defer log.Printf("Reducer %q finished", w.Name)
	// First, merge until there is no more mapping work to do.
	for {
		mergeTasks, err := w.client.Tasks(ctx, w.InputQueue)
		if err != nil {
			return fmt.Errorf("merge task list: %v", err)
		}
		log.Printf("Reducer %q found %d merge tasks", w.Name, len(mergeTasks))
		if len(mergeTasks) <= 1 {
			empty, err := w.client.QueuesEmpty(ctx, entroq.MatchExact(w.MapEmptyQueue))
			if err != nil {
				return fmt.Errorf("empty check in merge: %v", err)
			}
			if empty {
				log.Printf("Reducer %q nothing to merge", w.Name)
				break // all done - no more map tasks, 1 or fewer merge tasks.
			}

			// Nothing to do - sleep and continue.
			select {
			case <-ctx.Done():
				return fmt.Errorf("context canceled while shuffling")
			case <-time.After(5 * time.Second):
				log.Printf("Reducer %q trying again", w.Name)
			}
			continue
		}
		// More than one merge task is in the queue. Shuffle and check again.
		log.Printf("Reducer %q merging %d tasks", w.Name, len(mergeTasks))
		if err := w.mergeTasks(ctx, mergeTasks); err != nil {
			return fmt.Errorf("merge: %v", err)
		}
		log.Printf("Reducer %q merged %d tasks, pushing to %q", w.Name, len(mergeTasks), w.InputQueue)
	}

	log.Printf("Reducer %q merge finished. Reducing.", w.Name)

	// Then, reduce over the final merged task.
	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	task, err := w.client.Claim(claimCtx, w.InputQueue, claimDuration)
	if err != nil {
		return fmt.Errorf("reduce run claim: %v", err)
	}
	if err := w.reduceTask(ctx, task); err != nil {
		return fmt.Errorf("reduce run: %v", err)
	}
	return nil
}

// MapReduce creates and runs a full mapreduce pipeline, using EntroQ as its
// state storage under the given queue prefix. It spawns in-memory workers as
// goroutines, using the provided worker counts.
type MapReduce struct {
	client *entroq.EntroQ

	QueuePrefix string
	NumMappers  int
	NumReducers int

	Map    Mapper
	Reduce Reducer

	Data []*KV
}

// MapReduceOption modifies how a MapReduce is created.
type MapReduceOption func(*MapReduce)

// WithMap instructs the mapreduce to use the given mapper. Defaults to IdentityMapper.
func WithMap(m Mapper) MapReduceOption {
	return func(mr *MapReduce) {
		mr.Map = m
	}
}

// WithReduce instructs the mapreduce to use the given reducer. Defaults to NilReducer.
func WithReduce(r Reducer) MapReduceOption {
	return func(mr *MapReduce) {
		mr.Reduce = r
	}
}

// WithNumMappers sets the number of map workers.
func WithNumMappers(n int) MapReduceOption {
	if n < 1 {
		n = 1
	}
	return func(mr *MapReduce) {
		mr.NumMappers = n
	}
}

// WithNumReducers sets the number of reduce workers.
func WithNumReducers(n int) MapReduceOption {
	if n < 1 {
		n = 1
	}
	return func(mr *MapReduce) {
		mr.NumReducers = n
	}
}

// AddInput adds an input KV to a mapreduce.
func AddInput(kvs ...*KV) MapReduceOption {
	return func(mr *MapReduce) {
		mr.Data = append(mr.Data, kvs...)
	}
}

// NewMapReduce creates a mapreduce pipeline config, from which workers can be
// started to carry out the desired data manipulations.
func NewMapReduce(eq *entroq.EntroQ, qPrefix string, opts ...MapReduceOption) *MapReduce {
	mr := &MapReduce{
		client:      eq,
		QueuePrefix: qPrefix,
		NumMappers:  1,
		NumReducers: 1,
		Map:         IdentityMapper,
		Reduce:      NilReducer,
	}
	for _, opt := range opts {
		opt(mr)
	}
	return mr
}

// Run starts the mapreduce pipeline, adding data to EntroQ and starting workers.
// Returns the output queue with finished tasks.
func (mr *MapReduce) Run(ctx context.Context) (string, error) {
	var (
		qMap          = path.Join(mr.QueuePrefix, "map")
		qMapInput     = path.Join(qMap, "input")
		qReduce       = path.Join(mr.QueuePrefix, "reduce")
		qReduceInput  = path.Join(qReduce, "input")
		qReduceOutput = path.Join(qReduce, "output")
	)

	log.Printf("Creating tasks for inputs")
	// First create map tasks for all of the data.
	for _, kv := range mr.Data {
		b, err := json.Marshal(kv)
		if err != nil {
			return "", fmt.Errorf("marshal input: %v", err)
		}
		if _, _, err := mr.client.Modify(ctx, entroq.InsertingInto(qMapInput, entroq.WithValue(b))); err != nil {
			return "", fmt.Errorf("insert map input: %v", err)
		}
	}
	log.Printf("Created %d tasks", len(mr.Data))

	// When all tasks are present, start map and reduce workers. They'll all exit when finished.
	g, ctx := errgroup.WithContext(ctx)

	for i := 0; i < mr.NumMappers; i++ {
		i := i
		worker := NewMapWorker(mr.client, qMapInput,
			func() MapEmitter { return NewCollectingMapEmitter(mr.NumReducers) },
			MapAsName(fmt.Sprint(i)),
			MapToOutputPrefix(qReduceInput),
			WithMapper(mr.Map))

		g.Go(func() error {
			return worker.Run(ctx)
		})
	}

	for i := 0; i < mr.NumReducers; i++ {
		name := fmt.Sprint(i)
		worker := NewReduceWorker(mr.client, qMapInput, path.Join(qReduceInput, name),
			ReduceAsName(name),
			WithReducer(mr.Reduce),
			ReduceToOutput(qReduceOutput))

		g.Go(func() error {
			return worker.Run(ctx)
		})
	}

	log.Printf("Waiting for mappers and reducers to finish")
	if err := g.Wait(); err != nil {
		return "", fmt.Errorf("pipeline error: %v", err)
	}

	return qReduceOutput, nil
}
