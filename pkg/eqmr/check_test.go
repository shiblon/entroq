package eqmr_test

import (
	"context"
	"math/rand"
	"reflect"
	"strconv"
	"testing"
	"testing/quick"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/eqmr"
	"github.com/shiblon/entroq/pkg/eqmr/eqmrtest"
)

// checkConfig is the shared shape for these runs: intervals tightened so the
// suite is not paced by a production-sized control loop.
func checkConfig(unique, wordsPerDoc, docs, mapShards, reduceShards, mappers, reducers int) eqmrtest.Config {
	return eqmrtest.Config{
		UniqueWords:     unique,
		WordsPerDoc:     wordsPerDoc,
		NumDocs:         docs,
		MapShards:       mapShards,
		ReduceShards:    reduceShards,
		Mappers:         mappers,
		Reducers:        reducers,
		Lease:           5 * time.Second,
		ControlInterval: 20 * time.Millisecond,
	}
}

// TestCheckRandomizedShapes is the eqmr counterpart of the older
// examples/mr quick.Check test: run the pipeline over a generated corpus at
// randomized shapes and require the output to equal the histogram exactly.
//
// It varies more than the original could, because shard counts are now explicit
// knobs rather than consequences of the input size, and every combination must
// produce identical results.
func TestCheckRandomizedShapes(t *testing.T) {
	ctx := context.Background()

	cfg := &quick.Config{
		MaxCount: 6,
		Values: func(values []reflect.Value, rng *rand.Rand) {
			values[0] = reflect.ValueOf(rng.Intn(10) + 10) // docs
			values[1] = reflect.ValueOf(rng.Intn(8) + 1)   // map shards
			values[2] = reflect.ValueOf(rng.Intn(8) + 1)   // reduce shards
			values[3] = reflect.ValueOf(rng.Intn(4) + 1)   // mappers
			values[4] = reflect.ValueOf(rng.Intn(3) + 1)   // reducers
		},
	}

	check := func(docs, mapShards, reduceShards, mappers, reducers int) bool {
		eq, err := entroq.New(ctx, eqmem.Opener())
		if err != nil {
			t.Fatalf("open client: %v", err)
		}
		defer eq.Close()

		c := checkConfig(50, 200, docs, mapShards, reduceShards, mappers, reducers)
		if err := eqmrtest.Check(ctx, eq, c); err != nil {
			t.Errorf("docs=%d map=%d reduce=%d mappers=%d reducers=%d: %v",
				docs, mapShards, reduceShards, mappers, reducers, err)
			return false
		}
		return true
	}

	if err := quick.Check(check, cfg); err != nil {
		t.Fatal(err)
	}
}

// TestCheckManyDistinctWords covers the case the older check never did: enough
// distinct keys that they must genuinely spread across reduce partitions. The
// old implementation created one reduce task per distinct key, so this shape was
// the one it scaled worst on.
func TestCheckManyDistinctWords(t *testing.T) {
	ctx := context.Background()

	for _, unique := range []int{1000, 20000} {
		t.Run(strconv.Itoa(unique), func(t *testing.T) {
			eq, err := entroq.New(ctx, eqmem.Opener())
			if err != nil {
				t.Fatalf("open client: %v", err)
			}
			defer eq.Close()

			c := checkConfig(unique, 1000, 60, 6, 5, 4, 3)
			start := time.Now()
			if err := eqmrtest.Check(ctx, eq, c); err != nil {
				t.Fatal(err)
			}
			t.Logf("%d distinct words over %d docs: %s", unique, c.NumDocs,
				time.Since(start).Round(time.Millisecond))
		})
	}
}

// TestCheckCombinerParity runs the same corpus with and without a combiner and
// requires both to satisfy the histogram, so the combiner cannot quietly change
// an answer at any shard shape.
func TestCheckCombinerParity(t *testing.T) {
	ctx := context.Background()
	for _, comb := range []eqmr.Combiner{nil, eqmr.SumCombiner} {
		eq, err := entroq.New(ctx, eqmem.Opener())
		if err != nil {
			t.Fatalf("open client: %v", err)
		}
		c := checkConfig(500, 500, 24, 4, 6, 3, 2)
		c.Combiner = comb
		if err := eqmrtest.Check(ctx, eq, c); err != nil {
			t.Errorf("combiner=%v: %v", comb != nil, err)
		}
		eq.Close()
	}
}
