package eqmr

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// KV is a key/value pair: the input type for mappers and the output type of a
// completed run.
//
// Both halves must be valid UTF-8 with no NUL, the rule ValidateText applies.
// Content is stored as JSONB in PostgreSQL, which rejects NUL, and a JSON
// string cannot represent invalid UTF-8. Encode binary keys or values in the
// job that needs them.
type KV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// NewKV creates a key/value pair.
func NewKV(key, value string) *KV { return &KV{Key: key, Value: value} }

// String renders a readable form for logs and test failures.
func (kv *KV) String() string { return fmt.Sprintf("(%s)=%s", kv.Key, kv.Value) }

// EmitFunc is handed to a Mapper to emit intermediate pairs. It rejects a key or
// value that is not valid NUL-free UTF-8.
type EmitFunc func(ctx context.Context, key, value string) error

// Mapper is called once per input KV and emits zero or more intermediate pairs.
//
// Returning an error kills the worker, per the MapReduce contract: a run in
// which some map calls failed cannot support a claim about its output. The task
// is reclaimed once the lease expires, so a transient failure costs one claim,
// and a record that fails every time exhausts the claim ceiling and fails the
// run with a reason. To tolerate a particular failure, handle it in the Mapper
// and emit nothing.
type Mapper func(ctx context.Context, key, value string, emit EmitFunc) error

// Reducer is called once per distinct intermediate key with every value for
// that key, and returns the single value recorded in the run's output.
type Reducer func(ctx context.Context, input ReducerInput) (string, error)

// Combiner shrinks the value list for one key without finishing the reduction.
//
// Its output has the same type as its input, so it may run more than once and
// on data it has already touched. The contract is that combining in pieces
// equals combining all at once: for any split of the values into groups a and
// b, C(C(a) ++ C(b)) must equal C(a ++ b). Sum, min, max, count and set-union
// satisfy that; mean and median satisfy it only when carried as a richer
// intermediate value, such as a running sum and count.
//
// A Combiner is an optimization. Output is identical without one; intermediate
// data is larger. See SumCombiner.
type Combiner func(ctx context.Context, key string, values []string) ([]string, error)

// ReducerInput iterates the values for one intermediate key.
type ReducerInput interface {
	// Key is the intermediate key being reduced. Always available.
	Key() string
	// Value is the current value. Call Next before the first use.
	Value() string
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
	key    string
	values []string
	idx    int
}

func (s *sliceInput) Key() string { return s.key }
func (s *sliceInput) Err() error  { return nil }

func (s *sliceInput) Value() string {
	if s.idx == 0 || s.idx > len(s.values) {
		return ""
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

// Fingerprint64 produces a 64-bit unsigned integer from a string.
func Fingerprint64(key string) uint64 {
	h := fnv.New64()
	h.Write([]byte(key))
	return h.Sum64()
}

// ShardForKey assigns an intermediate key to one of n partitions. Every mapper
// in a run must agree on n, which is why Config.ReduceShards is required and
// fixed for the life of a run.
func ShardForKey(key string, n int) int {
	return int(Fingerprint64(key) % uint64(n))
}

// IdentityMapper emits its input unchanged.
func IdentityMapper(ctx context.Context, key, value string, emit EmitFunc) error {
	return emit(ctx, key, value)
}

// WordCountMapper emits word:count for each whitespace-separated word in the
// value, ignoring the input key. Splitting is naive on purpose.
func WordCountMapper(ctx context.Context, _, value string, emit EmitFunc) error {
	words := make(map[string]int)
	for w := range strings.FieldsSeq(value) {
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
		if err := emit(ctx, word, strconv.Itoa(count)); err != nil {
			return fmt.Errorf("word count emit: %w", err)
		}
		emitted++
	}
	return nil
}

// SumReducer sums decimal integer values for a key.
func SumReducer(_ context.Context, input ReducerInput) (string, error) {
	sum := 0
	for input.Next() {
		n, err := strconv.Atoi(input.Value())
		if err != nil {
			return "", fmt.Errorf("SumReducer int conversion: %w", err)
		}
		sum += n
	}
	if err := input.Err(); err != nil {
		return "", fmt.Errorf("SumReducer input: %w", err)
	}
	return strconv.Itoa(sum), nil
}

// SumCombiner is the Combiner matching SumReducer: it collapses a value list to
// a single running total. Addition is associative and commutative, so combining
// partial sums and then summing those equals summing everything at once, which
// is exactly the property Combiner requires.
func SumCombiner(_ context.Context, _ string, values []string) ([]string, error) {
	if len(values) < 2 {
		return values, nil
	}
	sum := 0
	for _, v := range values {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("SumCombiner int conversion: %w", err)
		}
		sum += n
	}
	return []string{strconv.Itoa(sum)}, nil
}

// FirstValueReducer returns the first value for a key and stops.
func FirstValueReducer(_ context.Context, input ReducerInput) (string, error) {
	if !input.Next() {
		return "", fmt.Errorf("no inputs to reducer")
	}
	if err := input.Err(); err != nil {
		return "", fmt.Errorf("FirstValueReducer input: %w", err)
	}
	return input.Value(), nil
}

// EmptyReducer produces an empty value for every key.
func EmptyReducer(_ context.Context, _ ReducerInput) (string, error) { return "", nil }

// SliceReducer produces a JSON-serialized slice of all values for a key.
func SliceReducer(_ context.Context, input ReducerInput) (string, error) {
	var vals []string
	for input.Next() {
		vals = append(vals, input.Value())
	}
	if err := input.Err(); err != nil {
		return "", fmt.Errorf("SliceReducer input: %w", err)
	}
	b, err := json.Marshal(vals)
	if err != nil {
		return "", fmt.Errorf("SliceReducer marshal: %w", err)
	}
	return string(b), nil
}
