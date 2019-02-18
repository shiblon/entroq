// Package mrtest is a test package tightly tied to the mr package, separated
// out to avoid import cycles when other tests want to use it.
package mrtest // import "entrogo.com/entroq/contrib/mrtest"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strings"

	"entrogo.com/entroq"
	. "entrogo.com/entroq/contrib/mr"
	"github.com/google/uuid"
)

// MRCheck is a check function that runs a mapreduce using the specified number of mappers and reducers.
//
// Creates a bunch of documents, filled with numeric strings (words are all
// numbers, makes things easy). We start with a known histogram, then we
// assemble documents by randomly drawing without replacement until the
// distribution is empty. That way we know our outputs from the beginning,
// and we get a random starting point.
//
// When using with quick.Check, the first two arguments should usually be
// fixed, but the remainder can be "checked". Thus, it often makes sense to use
// it in a closure, thus:
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
	const (
		uniqueWords = 10
		wordsPerDoc = 1000
	)

	log.Printf("Checking MR with docs=%d, mappers=%d, reducers=%d", numDocs, numMappers, numReducers)
	// Flattened slice of words (keep track in a histogram, too).
	// Random histogram of "words" (integers).
	var occurrences []string
	histogram := make(map[string]int)
	for i := 0; i < wordsPerDoc*numDocs; i++ {
		val := fmt.Sprint(rand.Intn(uniqueWords))
		histogram[val]++
		occurrences = append(occurrences, val)
	}

	// Shuffle occurrences, then make "documents" out of them.
	rand.Shuffle(len(occurrences), func(i, j int) {
		occurrences[i], occurrences[j] = occurrences[j], occurrences[i]
	})

	var docs []*KV
	for di := 0; di < numDocs; di++ {
		docs = append(docs, NewKV(nil, []byte(strings.Join(occurrences[di*wordsPerDoc:(di+1)*wordsPerDoc], " "))))
	}

	// Create expected output from processing these documents. Should be sorted
	// (lexicographical) list of words with their frequencies.
	var expected []*KV
	for word, count := range histogram {
		expected = append(expected, NewKV([]byte(word), []byte(fmt.Sprint(count))))
	}
	sort.Slice(expected, func(i, j int) bool {
		return bytes.Compare(expected[i].Key, expected[j].Key) < 0
	})

	queuePrefix := "/mrtest/" + uuid.New().String()

	mr := NewMapReduce(eq, queuePrefix,
		WithNumMappers(numMappers),
		WithNumReducers(numReducers),
		WithMap(WordCountMapper),
		WithReduce(SumReducer),
		WithEarlyReduce(SumReducer),
		AddInput(docs...))

	outQ, err := mr.Run(ctx)
	if err != nil {
		log.Print(err)
		return false
	}

	tasks, err := eq.Tasks(ctx, outQ)
	if err != nil {
		log.Print(err)
		return false
	}

	good := true
	if len(tasks) == 0 || len(tasks) > numReducers {
		log.Printf("Expected between 1 and %d output tasks, got %d", numReducers, len(tasks))
		good = false
		for i, t := range tasks {
			var kvs []*KV
			if err := json.Unmarshal(t.Value, &kvs); err != nil {
				log.Printf("Failed to unmarshal task %v on queue %q: %v", t.IDVersion(), t.Queue, err)
				return false
			}
			log.Printf("%d: %q %v", i, t.Queue, t.IDVersion())
			kvmap := make(map[string]string)
			for _, kv := range kvs {
				kvmap[string(kv.Key)] = string(kv.Value)
			}
			out, err := json.Marshal(kvmap)
			if err != nil {
				log.Printf("Failed to marshal kvmap: %v", err)
			}
			log.Print(string(out))
		}
	}

	// Check that each task's values are already in key-sorted order.
	var allKVs []*KV
	for i, task := range tasks {
		var got []*KV
		if err := json.Unmarshal(task.Value, &got); err != nil {
			log.Print(err)
			return false
		}
		if !sort.SliceIsSorted(got, func(i, j int) bool {
			return bytes.Compare(got[i].Key, got[j].Key) < 0
		}) {
			log.Printf("reduce task %d contains unsorted keys: %v", i, got)
			return false
		}
		allKVs = append(allKVs, got...)
	}

	sort.Slice(allKVs, func(i, j int) bool {
		return bytes.Compare(allKVs[i].Key, allKVs[j].Key) < 0
	})

	for i, kv := range allKVs {
		if want, got := expected[i].String(), kv.String(); want != got {
			log.Printf("Expected %s, got %s", want, got)
			good = false
		}
	}
	if !good {
		queues, err := eq.Queues(ctx, entroq.MatchPrefix(queuePrefix))
		for q, n := range queues {
			log.Printf("queue %q = %d", q, n)
		}
		if err != nil {
			log.Printf("queues error: %v", err)
		}
	}
	return good
}
