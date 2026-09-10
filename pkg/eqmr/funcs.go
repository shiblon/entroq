package eqmr

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// KV is a key/value pair. It is the input type for mappers and the output type
// of a completed run. Both halves are []byte, so they may hold arbitrary bytes:
// encoding/json represents a []byte as base64, which round-trips losslessly,
// unlike a Go string containing invalid UTF-8.
type KV struct {
	Key   []byte `json:"key"`
	Value []byte `json:"value"`
}

// NewKV creates a key/value pair.
func NewKV(key, value []byte) *KV { return &KV{Key: key, Value: value} }

// String renders a readable form for logs and test failures.
func (kv *KV) String() string { return fmt.Sprintf("(%s)=%s", kv.Key, kv.Value) }

// EmitFunc is handed to a Mapper to emit intermediate pairs.
type EmitFunc func(ctx context.Context, key, value []byte) error

// Mapper is called once per input KV and emits zero or more intermediate pairs.
//
// A Mapper that returns an error kills its worker. That is deliberate and it is
// the standard MapReduce contract: a run in which some map calls failed cannot
// support any claim about the correctness of its output. The task itself is
// reclaimed once the lease expires, so a transient failure such as a network
// blip costs one claim rather than the run.
//
// Bounding that recovery is the caller's job, via RunOptions.MaxClaims (or
// worker.WithMaxClaims when running workers directly). A fatal handler error
// does not mark the task, so without a claim bound a genuinely poisonous record
// is retried forever. With one, the task is quarantined after a few tries and
// the controller fails the run with a reason.
//
// If a particular failure should be tolerated instead, that judgment belongs
// inside the Mapper body, which can swallow it and emit nothing.
type Mapper func(ctx context.Context, key, value []byte, emit EmitFunc) error

// Reducer is called once per distinct intermediate key with every value for
// that key, and returns the single value recorded in the run's output.
type Reducer func(ctx context.Context, input ReducerInput) ([]byte, error)

// Combiner shrinks the value list for one key without finishing the reduction.
//
// It differs from Reducer in exactly the way that matters: a Combiner's output
// has the same type as its input, so it is closed over its own output and may
// run repeatedly, at more than one stage, on data it has already touched. A
// Reducer collapses to a single value and can run only once, at the end.
//
// The contract a Combiner must satisfy is that combining in pieces equals
// combining all at once: for any partition of the values into groups a and b,
// C(C(a) ++ C(b)) must be equivalent to C(a ++ b). Sum, min, max, count, and
// set-union satisfy this; mean and median do not, unless carried as a richer
// intermediate value.
//
// Combiners are an optimization and never a correctness requirement: a run with
// no Combiner produces the same output, more slowly and with larger spills.
type Combiner func(ctx context.Context, key []byte, values [][]byte) ([][]byte, error)

// ReducerInput iterates the values for one intermediate key.
type ReducerInput interface {
	// Key is the intermediate key being reduced. Always available.
	Key() []byte
	// Value is the current value. Call Next before the first use.
	Value() []byte
	// Err returns any iteration error. Check it after Next returns false.
	Err() error
	// Next advances to the next value, reporting false when exhausted.
	//
	//	for input.Next() {
	//		process(input.Value())
	//	}
	//	if err := input.Err(); err != nil { ... }
	Next() bool
}

// sliceInput is a ReducerInput over an in-memory value slice.
type sliceInput struct {
	key    []byte
	values [][]byte
	idx    int
}

func (s *sliceInput) Key() []byte { return s.key }
func (s *sliceInput) Err() error  { return nil }

func (s *sliceInput) Value() []byte {
	if s.idx == 0 || s.idx > len(s.values) {
		return nil
	}
	return s.values[s.idx-1]
}

func (s *sliceInput) Next() bool {
	if s.idx >= len(s.values) {
		return false
	}
	s.idx++
	return true
}

// Fingerprint64 produces a 64-bit unsigned integer from a byte string.
func Fingerprint64(key []byte) uint64 {
	h := fnv.New64()
	h.Write(key)
	return h.Sum64()
}

// ShardForKey assigns an intermediate key to one of n partitions. Every mapper
// in a run must agree on n, which is why Config.ReduceShards is required and
// fixed for the life of a run.
func ShardForKey(key []byte, n int) int {
	return int(Fingerprint64(key) % uint64(n))
}

// IdentityMapper emits its input unchanged.
func IdentityMapper(ctx context.Context, key, value []byte, emit EmitFunc) error {
	return emit(ctx, key, value)
}

// WordCountMapper emits word:count for each whitespace-separated word in the
// value, ignoring the input key. Splitting is naive on purpose.
func WordCountMapper(ctx context.Context, _, value []byte, emit EmitFunc) error {
	words := make(map[string]int)
	for w := range strings.FieldsSeq(string(value)) {
		words[w]++
	}
	emitted := 0
	for word, count := range words {
		if (emitted+1)%1000 == 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("canceled map operation: %w", ctx.Err())
			default:
			}
		}
		if err := emit(ctx, []byte(word), []byte(strconv.Itoa(count))); err != nil {
			return fmt.Errorf("word count emit: %w", err)
		}
		emitted++
	}
	return nil
}

// SumReducer sums decimal integer values for a key.
func SumReducer(_ context.Context, input ReducerInput) ([]byte, error) {
	sum := 0
	for input.Next() {
		n, err := strconv.Atoi(string(input.Value()))
		if err != nil {
			return nil, fmt.Errorf("SumReducer int conversion: %w", err)
		}
		sum += n
	}
	if err := input.Err(); err != nil {
		return nil, fmt.Errorf("SumReducer input: %w", err)
	}
	return []byte(strconv.Itoa(sum)), nil
}

// SumCombiner is the Combiner matching SumReducer: it collapses a value list to
// a single running total. Addition is associative and commutative, so combining
// partial sums and then summing those equals summing everything at once, which
// is exactly the property Combiner requires.
func SumCombiner(_ context.Context, _ []byte, values [][]byte) ([][]byte, error) {
	if len(values) < 2 {
		return values, nil
	}
	sum := 0
	for _, v := range values {
		n, err := strconv.Atoi(string(v))
		if err != nil {
			return nil, fmt.Errorf("SumCombiner int conversion: %w", err)
		}
		sum += n
	}
	return [][]byte{[]byte(strconv.Itoa(sum))}, nil
}

// FirstValueReducer returns the first value for a key and stops.
func FirstValueReducer(_ context.Context, input ReducerInput) ([]byte, error) {
	if !input.Next() {
		return nil, fmt.Errorf("no inputs to reducer")
	}
	if err := input.Err(); err != nil {
		return nil, fmt.Errorf("FirstValueReducer input: %w", err)
	}
	return input.Value(), nil
}

// NilReducer produces a nil value for every key.
func NilReducer(_ context.Context, _ ReducerInput) ([]byte, error) { return nil, nil }

// SliceReducer produces a JSON-serialized slice of all values for a key.
func SliceReducer(_ context.Context, input ReducerInput) ([]byte, error) {
	var vals [][]byte
	for input.Next() {
		vals = append(vals, input.Value())
	}
	if err := input.Err(); err != nil {
		return nil, fmt.Errorf("SliceReducer input: %w", err)
	}
	return json.Marshal(vals)
}
