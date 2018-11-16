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

// MapEmitter is passed to a map input processor so it can emit multiple
// outputs for a single input by calling it.
type MapEmitter func(ctx context.Context, key, value []byte) error

// MapProcessor is a function that accepts a key/value pair and emits zero or
// more key/value pairs for reducing.
type Mapper func(ctx context.Context, key, value []byte, output MapEmitter) error

// IdentityMapper produces the same output as its input.
func IdentityMapper(_ context.Context, key, value []byte, output MapEmitter) error {
	return output(key, value)
}

// WordCountMapper produces word:1 for each word in the value. The input key is
// ignored. Splitting is purely based on whitespace, and is quite naive.
func WordCountMapper(ctx context.Context, key, value []byte, output MapEmitter) error {
	words := make(map[string]int)
	for _, w := range strings.Fields(string(value)) {
		words[w]++
	}
	numEmitted := 0
	for word, count := range words {
		if (numEmitted+1)%1000 == 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("canceled map operation")
			default:
			}
		}
		if err := output([]byte(word), []byte(fmt.Sprint(count))); err != nil {
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

// MarshalJSON converts this key/value pair into JSON.
func (kv *KV) MarshalJSON() ([]byte, error) {
	return json.Marshal(kv)
}

// UnmarshalJSON attempts to parse JSON into this KV.
func (kv *KV) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, kv)
}

// UnmarshalKVJSON attempts to create a new KV from JSON data.
func UnmarshalKVJSON(data []byte) (*KV, error) {
	kv := new(KV)
	if err := kv.UnmarshalJSON(data); err != nil {
		return nil, err
	}
	return kv, nil
}

// MapWorker claims map input tasks, processes them, and produces output tasks.
type MapWorker struct {
	client *entroq.EntroQ

	ClaimantID uuid.UUID

	InputQueue   string
	OutputPrefix string
	NumReducers  int
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
func WithMapToOutputPrefix(p string) MapWorkerOption {
	return func(mw *MapWorker) {
		if p == "" {
			return
		}
		mw.OutputPrefix = p
	}
}

// MapToReducers changes the number of output queues the mapper can use to
// send completion tasks to. The default for this value is 1 reducer.
func MapToReducers(n int) MapWorkerOption {
	return func(mw *MapWorker) {
		if n < 1 {
			n = 1
		}
		mw.NumReducers = n
	}
}

// NewMapWorker creates a new MapWorker, which loops until told to stop,
// claiming tasks and processing them, placing them into an appropriate output
// queue calculated from the output key.
func NewMapWorker(eq *entroq.EntroQ, inQueue string, opts ...MapWorkerOption) *MapWorker {
	w := &MapWorker{
		client:       eq,
		ClaimantID:   uuid.New(),
		InputQueue:   inQueue,
		OutputPrefix: inQueue,
		Map:          IdentityMapper,
		NumReducers:  1,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

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

	emitter := &CollectingMapEmitter{
		NumShards: numShards,
	}
	for i := 0; i < numShards; i++ {
		emitter.shards = append(emitter.shards, nil)
	}

	return emitter
}

// Emit adds a new key/value pair to the emitter.
func (e *CollectingMapEmitter) Emit(_ context.Context, key, value []byte) error {
	shard := ShardForKey(key)
	e.shards[shard] = append(e.shards[shard], NewKV(key, value))
}

// AsModifyArgs returns a slice of arguments to be sent to insert new shuffle
// tasks after emissions are complete. Additional modifications can be passed in
// to make, e.g., simultaneous task deletion easier to specify.
func (e *CollectingMapEmitter) AsModifyArgs(qPrefix string, additional ...entroq.ModifyArg) ([][]*KV, error) {
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

// mapTask runs a mapper over a given task's input. It does everything in
// memory, including storage of outputs. Note that, were we to want this to
// scale, we would need to write output to a number of temporary files, then
// clean them up if there were an error of any kind (or let them get garbage
// collected). They would need to not be referenced unless the entire map task
// were completed, at which point they would be pointed to by the completion
// task. That's very doable, but beyond the scope of this exercise.
func (w *MapWorker) mapTask(ctx context.Context, task *entroq.Task) error {
	emitter := NewCollectingMapEmitter(w.NumReducers)

	finalTask, err := w.client.DoWithRenewal(ctx, task, time.Minute,
		func(ctx context.Context, id uuid.UUID, data *entroq.TaskData) error {
			kv, err := UnmarshalKVJSON(data.Value)
			if err != nil {
				return fmt.Errorf("map run json unmarshal: %v", err)
			}

			if err := w.Map(ctx, kv.Key, kv.Value, emitter.Emit); err != nil {
				return fmt.Errorf("map task %s failed: %v", id, err)
			}

			return nil
		})

	// Delete map task and create shuffle shard tasks.
	args := emitter.AsModifyArgs(w.OutputPrefix, finalTask.AsDeletion())
	if _, _, err := w.client.Modify(ctx, task.Claimant, modifyArgs...); err != nil {
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
		select {
		case <-ctx.Done():
			return fmt.Errorf("map run canceled")
		default:
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
}

// TODO:
// master:
// - read from input stream, getting keys and values
// - push all tasks to map queue
// - start mappers (this makes everything below much simpler - shufflers can just check for empty queue)
// - create shuffle tasks, one per reduce shard, place in shuffle queue
//
// shuffle worker:
// - claim a shuffle shard task from the queue of shard tasks (one per shard, while the mappers are still working, then while there's more than one thing in the queue)
// - pull data from all tasks in the shard queue, sort it all together. Delete all and add new combined task.
// - if map queue is empty, delete shard task and move data task into reducer fanout queue.
//
// reducer fanout worker:
// - claim finished shuffle task
// - split into unique keys
// - delete shuffle task, add all reduce tasks at once.
//
// reduce worker:
// - claim a reduce task
// - process all values
// - delete task and write to output stream
