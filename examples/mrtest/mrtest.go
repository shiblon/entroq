// Package mrtest provides test helpers for the mr MapReduce example.
// It is a separate package (rather than mr_test) because test files are not
// importable, and other test packages need MRCheck to run MapReduce correctness checks.
package mrtest

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strings"

	"github.com/shiblon/entroq"
	. "github.com/shiblon/entroq/examples/mr"
)

// MRCheck runs a MapReduce with the given parameters and verifies that the
// output matches the expected word-count histogram.
//
// Creates numDocs documents filled with numeric strings (words are integers).
// Builds a known histogram first, then shuffles words into documents. After
// running MapReduce, checks that the result docs match the histogram exactly.
//
// When using with quick.Check, the first two arguments should usually be
// fixed, but the remainder can be "checked". Thus, it often makes sense to use
// it in a closure:
//
//	config := &quick.Config{
//		MaxCount: 5,
//		Values: func(values []reflect.Value, rand *rand.Rand) {
//			values[0] = reflect.ValueOf(rand.Intn(2000) + 1000)
//			values[1] = reflect.ValueOf(rand.Intn(100) + 1)
//			values[2] = reflect.ValueOf(rand.Intn(20) + 1)
//		},
//	}
//	check := func(nm, nr int) bool {
//		return MRCheck(ctx, client, nm, nr)
//	}
//	if err := quick.Check(check, config); err != nil {
//		t.Fatal(err)
//	}
func MRCheck(ctx context.Context, eq *entroq.EntroQ, numDocs, numMappers, numReducers int) bool {
	return MRCheckAt(ctx, eq, "/mrtest/"+entroq.GenHex16(), numDocs, numMappers, numReducers)
}

// MRCheckAt is MRCheck with a caller-supplied queue and document prefix.
func MRCheckAt(ctx context.Context, eq *entroq.EntroQ, queuePrefix string, numDocs, numMappers, numReducers int) bool {
	const (
		uniqueWords = 10
		wordsPerDoc = 1000
	)

	log.Printf("Checking MR with docs=%d, mappers=%d, reducers=%d", numDocs, numMappers, numReducers)

	// Build a random histogram of "words" (integers), then shuffle into docs.
	var occurrences []string
	histogram := make(map[string]int)
	for i := 0; i < wordsPerDoc*numDocs; i++ {
		val := fmt.Sprint(rand.Intn(uniqueWords))
		histogram[val]++
		occurrences = append(occurrences, val)
	}
	rand.Shuffle(len(occurrences), func(i, j int) {
		occurrences[i], occurrences[j] = occurrences[j], occurrences[i]
	})

	var docs []*KV
	for di := range numDocs {
		docs = append(docs, NewKV(nil, []byte(strings.Join(occurrences[di*wordsPerDoc:(di+1)*wordsPerDoc], " "))))
	}

	// Expected: sorted list of (word, count) pairs.
	var expected []*KV
	for word, count := range histogram {
		expected = append(expected, NewKV([]byte(word), []byte(fmt.Sprint(count))))
	}
	sort.Slice(expected, func(i, j int) bool {
		return bytes.Compare(expected[i].Key, expected[j].Key) < 0
	})

	if err := RunAll(ctx, eq, queuePrefix, docs, WordCountMapper, SumReducer, numMappers, numReducers); err != nil {
		log.Print(err)
		return false
	}

	results, err := Results(ctx, eq, queuePrefix)
	if err != nil {
		log.Print(err)
		return false
	}

	if len(results) != len(expected) {
		log.Printf("Expected %d results, got %d", len(expected), len(results))
		return false
	}

	if !sort.SliceIsSorted(results, func(i, j int) bool {
		return bytes.Compare(results[i].Key, results[j].Key) < 0
	}) {
		log.Printf("results are not sorted by key: %v", results)
		return false
	}

	good := true
	for i, kv := range results {
		if want, got := expected[i].String(), kv.String(); want != got {
			log.Printf("Expected %s, got %s", want, got)
			good = false
		}
	}
	return good
}
