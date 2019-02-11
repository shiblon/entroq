package mem

import (
	"context"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/contrib/mrtest"
	"github.com/shiblon/entroq/qsvc/qtest"
)

func TestSimpleSequence(t *testing.T) {
	ctx := context.Background()

	client, stop, err := qtest.ClientService(ctx, Opener())
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer stop()

	qtest.SimpleSequence(ctx, t, client, "")
}

func TestSimpleWorker(t *testing.T) {
	ctx := context.Background()

	client, stop, err := qtest.ClientService(ctx, Opener())
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer stop()

	qtest.SimpleWorker(ctx, t, client, "")
}

func TestQueueMatch(t *testing.T) {
	ctx := context.Background()

	client, stop, err := qtest.ClientService(ctx, Opener())
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer stop()

	qtest.QueueMatch(ctx, t, client, "")
}

func TestMapReduce_checkLarge(t *testing.T) {
	config := &quick.Config{
		MaxCount: 5,
		Values: func(values []reflect.Value, rand *rand.Rand) {
			values[0] = reflect.ValueOf(rand.Intn(5000) + 5000)
			values[1] = reflect.ValueOf(rand.Intn(100) + 1)
			values[2] = reflect.ValueOf(rand.Intn(20) + 1)
		},
	}

	ctx := context.Background()
	check := func(ndocs, nm, nr int) bool {
		client, err := entroq.New(ctx, Opener())
		if err != nil {
			t.Fatalf("Open mem client: %v", err)
		}
		return mrtest.MRCheck(ctx, client, ndocs, nm, nr)
	}
	if err := quick.Check(check, config); err != nil {
		t.Fatal(err)
	}
}
