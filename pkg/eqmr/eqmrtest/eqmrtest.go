// Package eqmrtest provides correctness helpers for the experimental eqmr
// MapReduce, mirroring the word-count histogram check that examples/mrtest
// applied to the older example implementation.
//
// The generator is deterministic given a seed and returns document bodies as
// raw bytes rather than a package-specific KV type, so the same input can be fed
// to two different MapReduce implementations for comparison.
package eqmrtest

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/eqmr"
)

// Docs builds numDocs document bodies drawn from uniqueWords distinct words,
// wordsPerDoc words each, and returns the exact word histogram alongside them.
// The same seed always produces the same corpus.
//
// Words are zero-padded ("w000042") so lexical and numeric order agree, which
// makes the expected output trivial to construct and compare.
func Docs(uniqueWords, wordsPerDoc, numDocs int, seed int64) (bodies [][]byte, histogram map[string]int) {
	rng := rand.New(rand.NewSource(seed))

	total := wordsPerDoc * numDocs
	histogram = make(map[string]int, uniqueWords)
	occurrences := make([]string, 0, total)
	for range total {
		w := fmt.Sprintf("w%06d", rng.Intn(uniqueWords))
		histogram[w]++
		occurrences = append(occurrences, w)
	}
	rng.Shuffle(len(occurrences), func(i, j int) {
		occurrences[i], occurrences[j] = occurrences[j], occurrences[i]
	})

	bodies = make([][]byte, 0, numDocs)
	for i := range numDocs {
		bodies = append(bodies, []byte(strings.Join(occurrences[i*wordsPerDoc:(i+1)*wordsPerDoc], " ")))
	}
	return bodies, histogram
}

// VerifyHistogram checks that results are exactly the word counts in histogram,
// sorted by key. It returns a descriptive error rather than logging, so a caller
// can report the first real discrepancy.
func VerifyHistogram(results []*eqmr.KV, histogram map[string]int) error {
	if len(results) != len(histogram) {
		return fmt.Errorf("got %d distinct keys, want %d", len(results), len(histogram))
	}
	if !sort.SliceIsSorted(results, func(i, j int) bool {
		return bytes.Compare(results[i].Key, results[j].Key) < 0
	}) {
		return fmt.Errorf("results are not sorted by key")
	}
	for _, kv := range results {
		want, ok := histogram[string(kv.Key)]
		if !ok {
			return fmt.Errorf("unexpected key %q in results", kv.Key)
		}
		got, err := strconv.Atoi(string(kv.Value))
		if err != nil {
			return fmt.Errorf("key %q has non-numeric count %q: %w", kv.Key, kv.Value, err)
		}
		if got != want {
			return fmt.Errorf("key %q counted %d, want %d", kv.Key, got, want)
		}
	}
	return nil
}

// Config describes one correctness run. Shard counts are required by eqmr and
// deliberately have no defaults here either.
type Config struct {
	Prefix       string
	UniqueWords  int
	WordsPerDoc  int
	NumDocs      int
	MapShards    int
	ReduceShards int
	Mappers      int
	Reducers     int
	Combiner     eqmr.Combiner
	Seed         int64

	// Lease and ControlInterval are passed through to eqmr.Config. Zero leaves
	// the eqmr defaults, which are sized for deployment rather than for tests.
	Lease           time.Duration
	ControlInterval time.Duration
}

// Check runs a word-count MapReduce over a generated corpus and verifies the
// output against the histogram that produced it.
func Check(ctx context.Context, eq *entroq.EntroQ, cfg Config) error {
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = "/eqmrcheck/" + entroq.GenHex16()
	}
	bodies, histogram := Docs(cfg.UniqueWords, cfg.WordsPerDoc, cfg.NumDocs, cfg.Seed)

	input := make([]*eqmr.KV, len(bodies))
	for i, b := range bodies {
		input[i] = eqmr.NewKV(nil, b)
	}

	ctrl, err := eqmr.New(eq, eqmr.Config{
		Prefix:          prefix,
		MapShards:       cfg.MapShards,
		ReduceShards:    cfg.ReduceShards,
		Lease:           cfg.Lease,
		ControlInterval: cfg.ControlInterval,
	})
	if err != nil {
		return fmt.Errorf("eqmrtest: %w", err)
	}
	if err := ctrl.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
		Mappers:  cfg.Mappers,
		Reducers: cfg.Reducers,
		Combiner: cfg.Combiner,
	}); err != nil {
		return fmt.Errorf("eqmrtest run: %w", err)
	}
	results, err := ctrl.Results(ctx)
	if err != nil {
		return fmt.Errorf("eqmrtest results: %w", err)
	}
	if err := VerifyHistogram(results, histogram); err != nil {
		return fmt.Errorf("eqmrtest verify (docs=%d words=%d map=%d reduce=%d): %w",
			cfg.NumDocs, cfg.UniqueWords, cfg.MapShards, cfg.ReduceShards, err)
	}
	return nil
}
