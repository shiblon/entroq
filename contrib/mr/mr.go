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
	"golang.org/x/sync/errgroup"
)

const (
	taskRenewalInterval = 30 * time.Second
	taskRenewalDuration = 2 * taskRenewalInterval
)

// MapEmitter is passed to a map input processor so it can emit multiple
// outputs for a single input by calling it.
type MapEmitter func(key, value []byte) error

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

// ReduceShardForKey produces the reduce shard for a given byte slice and
// number of shards.
func ReduceShareForKey(key []byte, n int) int {
	return int(Fingerprint64(key) % uint64(n))
}

func (w *MapWorker) mapTask(ctx context.Context, task *entroq.Task) error {
	// The emit function just collects all outputs into a map from key
	// shard to values. This in-memory approach wouldn't work in a
	// large-scale system, but it's fine for getting an idea of how and
	// whether things work here.
	//
	// Note that, were we to want this to scale, we would need to write
	// output to a number of temporary files, then clean them up if there
	// were an error of any kind (or let them get garbage collected). They
	// would need to not be referenced unless the entire map task were
	// completed, at which point they would be pointed to by the completion
	// task. That's very doable, but beyond the scope of this exercise.

	// Stash so that we have it for errors without hitting race conditions.
	taskID := task.ID

	mapdata := new(KV)
	if err := json.Unmarshal(task.Value, mapdata); err != nil {
		return fmt.Errorf("map run json unmarshal: %v", err)
	}

	// Now that we have the ID stored, we can start trying to renew the task's
	// lease. We'll do this every 30 seconds.
	// NOTE: This *changes the task*, so the task has to be protected before
	// being used. This means that the group context must be canceled, then
	// the group waited on, before the task can be used again. All we really
	// need in here is the task's *data*, not the task itself, so that's okay.
	g, groupCtx := errgroup.WithContext(ctx)

	// Renew the task lease periodically.
	g.Go(func() error {
		for {
			select {
			case <-groupCtx.Done():
				return nil // cancelation is the natural outcome
			case <-time.After(taskRenewalInterval):
				var err error
				if task, err = w.client.RenewFor(groupCtx, task, taskRenewalDuration); err != nil {
					return fmt.Errorf("could not refresh task lease: %v", err)
				}
			}
		}
	})

	var modifyArgs []entroq.ModifyArg

	// Once this finishes, the lease context will be canceled, causing the
	// renewal loop to terminate.
	g.Go(func() error {
		byShard := make(map[uint64][]*KV)
		key, value := mapdata.Key, mapdata.Value
		emit := func(k, v []byte) error {
			fp := Fingerprint64(k)
			byShard[fp] = append(byShard[fp], &KV{Key: k, Value: v})
			return nil
		}
		if err := w.Map(groupCtx, key, value, emit); err != nil {
			return fmt.Errorf("map task %s failed: %v", taskID, err)
		}

		// Now that we have all of the outputs collected in the map, we can
		// do a little sorting and shuffling. The tasks set up for shuffling include
		// a key and a slice of sorted (by key) values, so we get that set up here.
		// And we create push tasks for all of them while we're at it, after sorting.
		for shard, kvs := range byShard {
			sort.Slice(kvs, func(i, j int) bool {
				return bytes.Compare(kvs[i].Key, kvs[j].Key) < 0
			})
			serialized, err := json.Marshal(kvs)
			if err != nil {
				return fmt.Errorf("map output marshal: %v", err)
			}
			modifyArgs = append(modifyArgs,
				entroq.InsertingInto(path.Join(w.OutputPrefix, fmt.Sprint(shard)),
					entroq.WithValue(serialized)))
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return fmt.Errorf("error mapping task %v: %v", taskID, err)
	}

	// Now that the wait has returned, the task isn't getting renewed. Delete.
	// Insert all new shuffle shard tasks.
	modifyArgs = append(modifyArgs, task.AsDeletion())
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
			empty, err := w.client.IsEmpty(ctx, w.InputQueue)
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
