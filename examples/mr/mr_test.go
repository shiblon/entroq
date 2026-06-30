// Package mr_test is a test package that uses the mr MapReduce implementation.
// It is relied on by other tests, so it needs to be its own package, otherwise
// it would create an import cycle back to mr through mrtest.
package mr_test

import (
	"context"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/shiblon/entroq"
	. "github.com/shiblon/entroq/examples/mr"
	"github.com/shiblon/entroq/examples/mrtest"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

func TestMapReduce_inMemorySmall(t *testing.T) {
	ctx := context.Background()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatal(err)
	}
	defer eq.Close()

	// word1 appears 4 times, word3/word4/word5/word7 twice, the rest once.
	input := []*KV{
		NewKV(nil, []byte("word1 word2 word3 word4")),
		NewKV(nil, []byte("word1 word3 word5 word7")),
		NewKV(nil, []byte("word1 word4 word7 wordA")),
		NewKV(nil, []byte("word1 word5 word9 wordE")),
	}
	if err := RunAll(ctx, eq, "/mrtest", input, WordCountMapper, SumReducer, 2, 1); err != nil {
		t.Fatal(err)
	}

	results, err := Results(ctx, eq, "/mrtest")
	if err != nil {
		t.Fatal(err)
	}

	expected := []*KV{
		NewKV([]byte("word1"), []byte("4")),
		NewKV([]byte("word2"), []byte("1")),
		NewKV([]byte("word3"), []byte("2")),
		NewKV([]byte("word4"), []byte("2")),
		NewKV([]byte("word5"), []byte("2")),
		NewKV([]byte("word7"), []byte("2")),
		NewKV([]byte("word9"), []byte("1")),
		NewKV([]byte("wordA"), []byte("1")),
		NewKV([]byte("wordE"), []byte("1")),
	}

	if len(results) != len(expected) {
		t.Fatalf("Expected %d results, got %d", len(expected), len(results))
	}
	for i, kv := range results {
		if kv.String() != expected[i].String() {
			t.Errorf("Expected %s, got %s", expected[i], kv)
		}
	}
}

func TestMapReduce_check(t *testing.T) {
	config := &quick.Config{
		MaxCount: 5,
		Values: func(values []reflect.Value, rand *rand.Rand) {
			values[0] = reflect.ValueOf(rand.Intn(10) + 10)
			values[1] = reflect.ValueOf(rand.Intn(10) + 10)
			values[2] = reflect.ValueOf(rand.Intn(2) + 1)
		},
	}

	ctx := context.Background()
	check := func(ndocs, nm, nr int) bool {
		client, err := entroq.New(ctx, eqmem.Opener())
		if err != nil {
			t.Fatalf("Open mem client: %v", err)
		}
		defer client.Close()
		return mrtest.MRCheck(ctx, client, ndocs, nm, nr)
	}
	if err := quick.Check(check, config); err != nil {
		t.Fatal(err)
	}
}
