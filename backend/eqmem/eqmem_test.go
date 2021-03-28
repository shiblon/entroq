package eqmem

import (
	"context"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"entrogo.com/entroq"
	"entrogo.com/entroq/contrib/mrtest"
	"entrogo.com/entroq/qsvc/qtest"
)

func RunQTest(t *testing.T, tester qtest.Tester) {
	t.Helper()
	ctx := context.Background()
	client, stop, err := qtest.ClientService(ctx, Opener())
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer stop()
	tester(ctx, t, client, "")
}

func TestEQMemSimpleSequence(t *testing.T) {
	RunQTest(t, qtest.SimpleSequence)
}

func TestEQMemTasksWithID(t *testing.T) {
	RunQTest(t, qtest.TasksWithID)
}

func TestEQMemTasksOmitValue(t *testing.T) {
	RunQTest(t, qtest.TasksOmitValue)
}

func TestEQMemTasksWithIDOnly(t *testing.T) {
	RunQTest(t, qtest.TasksWithIDOnly)
}

func TestEQMemInsertWithID(t *testing.T) {
	RunQTest(t, qtest.InsertWithID)
}

func TestEQMemSimpleChange(t *testing.T) {
	RunQTest(t, qtest.SimpleChange)
}

func TestEQMemSimpleWorker(t *testing.T) {
	RunQTest(t, qtest.SimpleWorker)
}

func TestEQMemMultiWorker(t *testing.T) {
	RunQTest(t, qtest.MultiWorker)
}

func TestEQMemWorkerDependencyHandler(t *testing.T) {
	RunQTest(t, qtest.WorkerDependencyHandler)
}

func TestEQMemWorkerMoveOnError(t *testing.T) {
	RunQTest(t, qtest.WorkerMoveOnError)
}

func TestEQMemWorkerRetryOnError(t *testing.T) {
	RunQTest(t, qtest.WorkerRetryOnError)
}

func TestEQMemWorkerRenewal(t *testing.T) {
	RunQTest(t, qtest.WorkerRenewal)
}

func TestEQMemQueueMatch(t *testing.T) {
	RunQTest(t, qtest.QueueMatch)
}

func TestEQMemQueueStats(t *testing.T) {
	RunQTest(t, qtest.QueueStats)
}

func TestEQMemMapReduce_checkSmall(t *testing.T) {
	config := &quick.Config{
		MaxCount: 2,
		Values: func(values []reflect.Value, rand *rand.Rand) {
			values[0] = reflect.ValueOf(5)
			values[1] = reflect.ValueOf(rand.Intn(2) + 1)
			values[2] = reflect.ValueOf(1)
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

func TestEQMemMapReduce_checkLarge(t *testing.T) {
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
