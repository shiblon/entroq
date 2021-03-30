package eqmem

import (
	"context"
	"math/rand"
	"os"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"entrogo.com/entroq"
	"entrogo.com/entroq/contrib/mrtest"
	"entrogo.com/entroq/qsvc/qtest"
	"github.com/google/go-cmp/cmp"
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

func TestEQMemMapReduce_checkHuge(t *testing.T) {
	config := &quick.Config{
		MaxCount: 5,
		Values: func(values []reflect.Value, rand *rand.Rand) {
			values[0] = reflect.ValueOf(rand.Intn(50000) + 5000)
			values[1] = reflect.ValueOf(rand.Intn(1000) + 1)
			values[2] = reflect.ValueOf(rand.Intn(200) + 1)
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

func TestEQMemJournalClaim(t *testing.T) {
	journalDir, err := os.MkdirTemp("", "eqjournal-")
	if err != nil {
		t.Fatalf("Error opening temp dir for journal: %v", err)
	}
	defer os.RemoveAll(journalDir)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, Opener(WithJournal(journalDir)))
	if err != nil {
		t.Fatalf("Error opening client at dir %q: %v", journalDir, err)
	}

	if _, _, err := eq.Modify(ctx,
		entroq.InsertingInto("/queue/of/tasks", entroq.WithValue([]byte("hey"))),
		entroq.InsertingInto("/queue/of/others", entroq.WithValue([]byte("other"))),
	); err != nil {
		t.Fatalf("Error adding task: %v", err)
	}

	if _, err := eq.Claim(ctx, entroq.From("/queue/of/tasks")); err != nil {
		t.Fatalf("Failed to claim from /queue/of/tasks: %v", err)
	}

	// Close and reopen, see that everything is still there.
	eq.Close()
	eq = nil

	if eq, err = entroq.New(ctx, Opener(WithJournal(journalDir))); err != nil {
		t.Fatalf("Error reopening client at dir %q: %v", journalDir, err)
	}

	expect := map[string]*entroq.QueueStat{
		"/queue/of/tasks": &entroq.QueueStat{
			Name:      "/queue/of/tasks",
			Claimed:   1,
			Available: 0,
			Size:      1,
			MaxClaims: 1,
		},
		"/queue/of/others": &entroq.QueueStat{
			Name:      "/queue/of/others",
			Claimed:   0,
			Available: 1,
			Size:      1,
			MaxClaims: 0,
		},
	}

	stats, err := eq.QueueStats(ctx)
	if err != nil {
		t.Fatalf("Queue stats: %v", err)
	}

	if diff := cmp.Diff(expect, stats); diff != "" {
		t.Errorf("Unexpected diff (-want +got):\n%v", diff)
	}
}
