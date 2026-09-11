// Package eqmrtest verifies eqmr pipelines against a known answer.
//
// Docs generates a word corpus together with its exact histogram, Check runs a
// word-count MapReduce over such a corpus and compares the two, and QuickCheck
// does the same at a size given by document and worker counts. Bodies come back
// as plain strings, so the same corpus can be fed to more than one
// implementation.
//
//	if err := eqmrtest.QuickCheck(ctx, eq, 100, 8, 3); err != nil {
//		t.Fatal(err)
//	}
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

// Intervals applied when a Config leaves them zero.
//
// The lease has to outlast a single unit of work. A reduce unit is a whole
// partition, so its duration grows with how much the map phase sent there; the
// value here covers the largest partition these helpers produce under a process
// running hundreds of workers.
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

	ctrl, err := eqmr.New(eq, prefix,
		eqmr.WithMapShards(cfg.MapShards),
		eqmr.WithReduceShards(cfg.ReduceShards),
		eqmr.WithLease(cfg.Lease),
		eqmr.WithControlInterval(cfg.ControlInterval),
	)
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

// Corpus shape for QuickCheck.
//
// A mapper emits one pair per distinct word per document, so intermediate
// volume is documents times distinct words, and callers pass document counts in
// the tens of thousands. The knob is steep: at 25 distinct words a 50,000
// document check runs in about five minutes, at 100 it produces partitions
// large enough for a reduce unit to outlive a 30 second lease, and at 500 even
// a 10,000 document check takes a minute and a half. Twenty-five spreads keys
// across the partition counts these callers use while keeping the corpus cheap.
//
// A test wanting deep fan-out should call Check with a larger UniqueWords and a
// bounded NumDocs.
const (
	quickUniqueWords = 25
	quickWordsPerDoc = 200

	// quickMaxSplits caps how finely QuickCheck divides its corpus.
	//
	// A split is a batch of records, and intermediate volume is splits times
	// distinct keys per split, so dividing a large corpus one record per split
	// multiplies the documents a run has to move without testing anything
	// further. Callers pass document counts in the tens of thousands; this
	// keeps the pipeline the subject of the test.
	quickMaxSplits = 512
)

// QuickCheck runs a word-count MapReduce sized by the given parameters and
// verifies it against the histogram that generated it.
//
// It exists so a cross-backend test can vary scale without composing a Config.
// Records are divided into at most quickMaxSplits batches, and partitions
// follow the reducer count.
func QuickCheck(ctx context.Context, eq *entroq.EntroQ, numDocs, numMappers, numReducers int) error {
	return Check(ctx, eq, Config{
		UniqueWords:  quickUniqueWords,
		WordsPerDoc:  quickWordsPerDoc,
		NumDocs:      numDocs,
		MapShards:    min(numDocs, quickMaxSplits),
		ReduceShards: numReducers,
		Mappers:      numMappers,
		Reducers:     numReducers,
		Seed:         int64(numDocs*1000 + numMappers*10 + numReducers),
	})
}
