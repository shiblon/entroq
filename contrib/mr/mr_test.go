// Package mr_test is a test package that uses the mr MapReduce implementation.
// It is relied on by other tests, so it needs to be its own package, otherwise
// it would create an import cycle back to mr through mrtest.
package mr_test

import (
	"context"
	"encoding/json"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/shiblon/entroq"
	. "github.com/shiblon/entroq/contrib/mr"
	"github.com/shiblon/entroq/contrib/mrtest"
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

func TestMapReduce_checkLarge(t *testing.T) {
	config := &quick.Config{
		MaxCount: 5,
		Values: func(values []reflect.Value, rand *rand.Rand) {
			//values[0] = reflect.ValueOf(rand.Intn(5000) + 5000)
			values[0] = reflect.ValueOf(10)
			//values[1] = reflect.ValueOf(rand.Intn(100) + 1)
			values[1] = reflect.ValueOf(10)
			//values[2] = reflect.ValueOf(rand.Intn(20) + 1)
			values[2] = reflect.ValueOf(2)
		},
	}

	ctx := context.Background()
	check := func(ndocs, nm, nr int) bool {
		client, err := entroq.New(ctx, mem.Opener())
		if err != nil {
			t.Fatalf("Open mem client: %v", err)
		}
		return mrtest.MRCheck(ctx, client, ndocs, nm, nr)
	}
	if err := quick.Check(check, config); err != nil {
		t.Fatal(err)
	}
}
