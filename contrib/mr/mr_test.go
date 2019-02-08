package mr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/quick"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/mem"
)

func TestMapReduce_inMemorySmall(t *testing.T) {
	ctx := context.Background()

	eq, err := entroq.New(ctx, mem.Opener())
	if err != nil {
		t.Fatal(err)
	}
	defer eq.Close()

	mr := NewMapReduce(eq, "/mrtest",
		WithNumMappers(2),
		WithNumReducers(1),
		WithMap(WordCountMapper),
		WithReduce(NilReducer),
		AddInput(NewKV(nil, []byte("word1 word2 word3 word4"))),
		AddInput(NewKV(nil, []byte("word1 word3 word5 word7"))),
		AddInput(NewKV(nil, []byte("word1 word4 word7 wordA"))),
		AddInput(NewKV(nil, []byte("word1 word5 word9 wordE"))))

	outQ, err := mr.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}

	tasks, err := eq.Tasks(ctx, outQ)
	if err != nil {
		t.Fatal(err)
	}

	expected := []*KV{
		NewKV([]byte("word1"), nil),
		NewKV([]byte("word2"), nil),
		NewKV([]byte("word3"), nil),
		NewKV([]byte("word4"), nil),
		NewKV([]byte("word5"), nil),
		NewKV([]byte("word7"), nil),
		NewKV([]byte("word9"), nil),
		NewKV([]byte("wordA"), nil),
		NewKV([]byte("wordE"), nil),
	}

	if len(tasks) != 1 {
		t.Fatalf("Expected 1 final reduced output task, got %d", len(tasks))
	}

	task := tasks[0]

	var kvs []*KV
	if err := json.Unmarshal(task.Value, &kvs); err != nil {
		t.Fatal(err)
	}

	for i, kv := range kvs {
		if kv.String() != expected[i].String() {
			t.Errorf("Expected %s, got %s", expected[i], kv)
		}
	}
}

// mrCheck is a check function that runs a mapreduce using the specified number of mappers and reducers.
//
// Creates a bunch of documents, filled with numeric strings (words are all
// numbers, makes things easy). We start with a known histogram, then we
// assemble documents by randomly drawing without replacement until the
// distribution is empty. That way we know our outputs from the beginning,
// and we get a random starting point.
func mrCheck(numMappers, numReducers int) bool {
	const (
		uniqueWords = 200
		wordsPerDoc = 1000
		numDocs     = 10000
	)
	ctx := context.Background()

	eq, err := entroq.New(ctx, mem.Opener())
	if err != nil {
		log.Printf("Failed to get client: %v", err)
		return false
	}

	log.Printf("Checking in-memory MR with %d mappers and %d reducers", numMappers, numReducers)
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

	mr := NewMapReduce(eq, "/mrtest",
		WithNumMappers(numMappers),
		WithNumReducers(numReducers),
		WithMap(WordCountMapper),
		WithReduce(SumReducer),
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

	if len(tasks) == 0 || len(tasks) > numReducers {
		log.Printf("Expected between 1 and %d output tasks, got %d", numReducers, len(tasks))
		for i, t := range tasks {
			var kvs []*KV
			if err := json.Unmarshal(t.Value, &kvs); err != nil {
				log.Printf("Failed to unmarshal task %v on queue %q: %v", t.IDVersion(), t.Queue, err)
				return false
			}
			log.Printf("%d: %v", i, t.IDVersion())
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
		return false
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
		if want, got := kv.String(), expected[i].String(); want != got {
			log.Printf("Expected %s, got %s", want, got)
			return false
		}
	}
	return true
}

func TestMapReduce_checkSmall(t *testing.T) {
	config := &quick.Config{
		MaxCount: 10,
		Values: func(values []reflect.Value, rand *rand.Rand) {
			values[0] = reflect.ValueOf(rand.Intn(20) + 1)
			values[1] = reflect.ValueOf(rand.Intn(5) + 1)
		},
	}

	if err := quick.Check(mrCheck, config); err != nil {
		t.Fatal(err)
	}
}

func TestMapReduce_checkMedium(t *testing.T) {
	config := &quick.Config{
		MaxCount: 8,
		Values: func(values []reflect.Value, rand *rand.Rand) {
			values[0] = reflect.ValueOf(rand.Intn(50) + 1)
			values[1] = reflect.ValueOf(rand.Intn(10) + 1)
		},
	}

	if err := quick.Check(mrCheck, config); err != nil {
		t.Fatal(err)
	}
}

func TestMapReduce_checkLarge(t *testing.T) {
	config := &quick.Config{
		MaxCount: 3,
		Values: func(values []reflect.Value, rand *rand.Rand) {
			values[0] = reflect.ValueOf(rand.Intn(200) + 1)
			values[1] = reflect.ValueOf(rand.Intn(50) + 1)
		},
	}

	if err := quick.Check(mrCheck, config); err != nil {
		t.Fatal(err)
	}
}
