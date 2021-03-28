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

func TestSimpleSequence(t *testing.T) {
	RunQTest(t, qtest.SimpleSequence)
}

func TestTasksWithID(t *testing.T) {
	RunQTest(t, qtest.TasksWithID)
}

func TestTasksOmitValue(t *testing.T) {
	RunQTest(t, qtest.TasksOmitValue)
}

func TestTasksWithIDOnly(t *testing.T) {
	RunQTest(t, qtest.TasksWithIDOnly)
}

func TestInsertWithID(t *testing.T) {
	RunQTest(t, qtest.InsertWithID)
}

func TestSimpleChange(t *testing.T) {
	RunQTest(t, qtest.SimpleChange)
}

func TestSimpleWorker(t *testing.T) {
	RunQTest(t, qtest.SimpleWorker)
}

func TestMultiWorker(t *testing.T) {
	RunQTest(t, qtest.MultiWorker)
}

func TestWorkerDependencyHandler(t *testing.T) {
	RunQTest(t, qtest.WorkerDependencyHandler)
}

func TestWorkerMoveOnError(t *testing.T) {
	RunQTest(t, qtest.WorkerMoveOnError)
}

func TestWorkerRetryOnError(t *testing.T) {
	RunQTest(t, qtest.WorkerRetryOnError)
}

func TestWorkerRenewal(t *testing.T) {
	RunQTest(t, qtest.WorkerRenewal)
}

func TestQueueMatch(t *testing.T) {
	RunQTest(t, qtest.QueueMatch)
}

func TestQueueStats(t *testing.T) {
	RunQTest(t, qtest.QueueStats)
}

func TestMapReduce_checkSmall(t *testing.T) {
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
