// Package eqmrtest provides correctness helpers for the experimental eqmr
// MapReduce, mirroring the word-count histogram check that examples/mrtest
// applied to the older example implementation.
//
// The generator is deterministic given a seed and returns document bodies as
// plain strings rather than a package-specific KV type, so the same input can be
// fed to two different MapReduce implementations for comparison.
package eqmrtest

import (
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
func Docs(uniqueWords, wordsPerDoc, numDocs int, seed int64) (bodies []string, histogram map[string]int) {
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

	bodies = make([]string, 0, numDocs)
	for i := range numDocs {
		bodies = append(bodies, strings.Join(occurrences[i*wordsPerDoc:(i+1)*wordsPerDoc], " "))
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
		return results[i].Key < results[j].Key
	}) {
		return fmt.Errorf("results are not sorted by key")
	}
	for _, kv := range results {
		want, ok := histogram[kv.Key]
		if !ok {
			return fmt.Errorf("unexpected key %q in results", kv.Key)
		}
		got, err := strconv.Atoi(kv.Value)
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

	// Lease and ControlInterval are passed through to eqmr.Config. Zero uses
	// the test defaults below rather than eqmr's, which are sized for a
	// deployment: a one-second control tick makes even a trivial check take
	// several seconds of pure waiting.
	Lease           time.Duration
	ControlInterval time.Duration
}

// Intervals used when a Config leaves them zero. These are test-shaped, not
// deployment-shaped: a check should be paced by the work, not by the control
// loop waiting to look again.
// A lease has to outlast a single unit of work, and a reduce unit here is a
// whole partition: its duration scales with how much the map phase sent there,
// not with some fixed per-record cost. Five seconds was enough until a large
// check put hundreds of workers in one process, where scheduling delay alone
// pushed renewals past expiry, tasks were reclaimed mid-work, and the claim
// ceiling correctly turned the churn into a failed run. Thirty gives room.
const (
	testLease           = 30 * time.Second
	testControlInterval = 25 * time.Millisecond
)

// Check runs a word-count MapReduce over a generated corpus and verifies the
// output against the histogram that produced it.
func Check(ctx context.Context, eq *entroq.EntroQ, cfg Config) error {
	if cfg.Lease == 0 {
		cfg.Lease = testLease
	}
	if cfg.ControlInterval == 0 {
		cfg.ControlInterval = testControlInterval
	}
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = "/eqmrcheck/" + entroq.GenHex16()
	}
	bodies, histogram := Docs(cfg.UniqueWords, cfg.WordsPerDoc, cfg.NumDocs, cfg.Seed)

	input := make([]*eqmr.KV, len(bodies))
	for i, b := range bodies {
		input[i] = eqmr.NewKV("", b)
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

// Standard shape for QuickCheck.
//
// The old examples/mrtest check fixed uniqueWords at 10, so no cross-backend
// test ever produced enough distinct keys to spread across reduce partitions:
// the shuffle went unexercised by the very tests meant to verify the pipeline.
//
// This value is chosen for cost, not maximised. The mapper emits one pair per
// distinct word per document, and callers here scale documents into the tens of
// thousands, so intermediate volume is documents times this number. At 500 the
// large eqmem check took 93 seconds; at 100 the huge one produced millions of
// spill documents and partitions so large that reduce units outlived their
// lease. Twenty-five still spreads keys across partitions, which is the
// coverage the old fixed value of 10 never gave, while staying near the old
// cost. Deep fan-out belongs in this package's own tests, which run to twenty
// thousand distinct keys against a bounded document count.
const (
	quickUniqueWords = 25
	quickWordsPerDoc = 200
)

// QuickCheck runs a word-count MapReduce sized by the given parameters and
// verifies it against the histogram that generated it.
//
// It exists so a cross-backend test can vary scale without composing a Config.
// Splits are one per input document, matching how a caller would naturally
// decompose this workload, and partitions follow the reducer count.
func QuickCheck(ctx context.Context, eq *entroq.EntroQ, numDocs, numMappers, numReducers int) error {
	return Check(ctx, eq, Config{
		UniqueWords:  quickUniqueWords,
		WordsPerDoc:  quickWordsPerDoc,
		NumDocs:      numDocs,
		MapShards:    numDocs,
		ReduceShards: numReducers,
		Mappers:      numMappers,
		Reducers:     numReducers,
		Seed:         int64(numDocs*1000 + numMappers*10 + numReducers),
	})
}
