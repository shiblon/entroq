package eqmem

import (
	"context"
	"fmt"
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

	tmpDir, err := os.MkdirTemp("", "memtest-")
	if err != nil {
		t.Fatalf("Temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	client, stop, err := qtest.ClientService(ctx, Opener(WithJournal(tmpDir)))
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
		"/queue/of/tasks": {
			Name:      "/queue/of/tasks",
			Claimed:   1,
			Available: 0,
			Size:      1,
		},
		"/queue/of/others": {
			Name:      "/queue/of/others",
			Claimed:   0,
			Available: 1,
			Size:      1,
		},
	}

	stats, err := eq.QueueStats(ctx)
	if err != nil {
		t.Fatalf("Queue stats: %v", err)
	}

	// Set MaxClaims because we have no idea whether things get claimed twice.
	for q, s := range expect {
		s.MaxClaims = stats[q].MaxClaims
	}

	if diff := cmp.Diff(expect, stats); diff != "" {
		t.Errorf("Unexpected diff (-want +got):\n%v", diff)
	}
}

func stressJournalStats(t *testing.T) {
	const maxQueueTasks = 5000
	journalDir, err := os.MkdirTemp("", "eqjournal-")
	if err != nil {
		t.Fatalf("Error opening temp dir for journal: %v", err)
	}
	defer os.RemoveAll(journalDir)

	ctx := context.Background()

	opener := Opener(WithJournal(journalDir), WithMaxJournalItems(100))

	// Create a journaled memory implementation with a relatively small journal max size.
	eq, err := entroq.New(ctx, opener)
	if err != nil {
		t.Fatalf("Error creating client: %v", err)
	}

	expectQueues := map[string]*entroq.QueueStat{
		"/queue/1": new(entroq.QueueStat),
		"/queue/2": new(entroq.QueueStat),
		"/queue/3": new(entroq.QueueStat),
	}

	// Do a bunch of random filling, keep track of how much we did.
	for q, s := range expectQueues {
		s.Name = q
		for i, n := 0, rand.Intn(maxQueueTasks); i < n; i++ {
			if _, _, err := eq.Modify(ctx, entroq.InsertingInto(q, entroq.WithValue([]byte(fmt.Sprintf("value %d", i))))); err != nil {
				t.Fatalf("Error inserting into %q: %v", q, err)
			}
			s.Size++
			s.Available++
		}
	}

	// Close and reopen, check stats.
	eq.Close()
	eq = nil

	if eq, err = entroq.New(ctx, opener); err != nil {
		t.Fatalf("Error opening client a second time: %v", err)
	}

	var statOpts []entroq.QueuesOpt
	for q := range expectQueues {
		statOpts = append(statOpts, entroq.MatchExact(q))
	}
	qstats, err := eq.QueueStats(ctx, statOpts...)
	if err != nil {
		t.Fatalf("Error getting queue stats: %v", err)
	}

	if diff := cmp.Diff(expectQueues, qstats); diff != "" {
		t.Fatalf("Unexpected diff (-want +got):\n%v", diff)
	}

	// Now do a few claims and moves.
	var qs []string
	for q := range expectQueues {
		qs = append(qs, q)
	}

	nextQ := func(i int) string {
		return qs[(i+1)%len(qs)]
	}

	for i, q := range qs {
		newQ := nextQ(i)
		for ci, cn := 0, rand.Intn(expectQueues[q].Size/2); ci < cn; ci++ {
			task, err := eq.Claim(ctx, entroq.From(q))
			if err != nil {
				t.Fatalf("Error claiming task: %v", err)
			}
			expectQueues[q].Claimed++
			expectQueues[q].Available--
			if _, _, err := eq.Modify(ctx, task.AsChange(entroq.QueueTo(newQ))); err != nil {
				t.Fatalf("Error moving from %q to %q: %v", q, newQ, err)
			}
			expectQueues[q].Size--
			expectQueues[q].Claimed--
			expectQueues[newQ].Size++
			expectQueues[newQ].Available++
		}
	}

	// Close, reload, check stats again.
	eq.Close()
	eq = nil

	if eq, err = entroq.New(ctx, opener); err != nil {
		t.Fatalf("Error opening client a third time: %v", err)
	}
	defer eq.Close()

	if qstats, err = eq.QueueStats(ctx, statOpts...); err != nil {
		t.Fatalf("Error getting queue stats: %v", err)
	}

	for q, s := range expectQueues {
		// We don't know what this will be beforehand.
		s.MaxClaims = qstats[q].MaxClaims
	}

	if diff := cmp.Diff(expectQueues, qstats); diff != "" {
		t.Fatalf("Unexpected diff (-want +got):\n%v", diff)
	}
}

func TestEQMem_stressJournalStats(t *testing.T) {
	N := 1
	for i := 0; i < N; i++ {
		stressJournalStats(t)
	}
}

func TestEQMem_journalClaimModClaim(t *testing.T) {
	// This test exercises a specific bug that was found in the journal stress
	// test, but produced it reliably.
	journalDir, err := os.MkdirTemp("", "eqjournal-")
	if err != nil {
		t.Fatalf("Error opening temp dir for journal: %v", err)
	}
	defer os.RemoveAll(journalDir)

	ctx := context.Background()

	opener := Opener(WithJournal(journalDir), WithMaxJournalItems(100))
	eq, err := entroq.New(ctx, opener)
	if err != nil {
		t.Fatalf("Error opening: %v", err)
	}

	if _, _, err := eq.Modify(ctx, entroq.InsertingInto("/queue/1")); err != nil {
		t.Fatalf("Error inserting: %v", err)
	}

	task, err := eq.Claim(ctx, entroq.From("/queue/1"))
	if err != nil {
		t.Fatalf("Error claiming: %v", err)
	}

	if _, _, err := eq.Modify(ctx, task.AsChange(entroq.QueueTo("/queue/2"))); err != nil {
		t.Fatalf("Error changing: %v", err)
	}

	if _, err := eq.Claim(ctx, entroq.From("/queue/2")); err != nil {
		t.Fatalf("Error claiming again: %v", err)
	}

	eq.Close()

	// Try opening and reading the journal again.
	eq, err = entroq.New(ctx, opener)
	if err != nil {
		t.Fatalf("Error opening again: %v", err)
	}
	defer eq.Close()
}

func TestEQMem_journalInsClaimClaimDel(t *testing.T) {
	// This pattern, when performed from the command line, caused claimant
	// mismatch issues when reading from the journal.
	// Basically, you insert, then claim, then shut down.
	// Then claim *again* for deletion, after At expiration (mimicking an eqc
	// clear without forcing). The final journal read will fail without the fix.
	//
	// The fix was to ensure that journal reads ignore "claim errors", meaning they
	// only fail if there is a true dependency error, and they don't have to wait
	// for tasks to expire to perform modifications even though the journal
	// process has its very own ID.
	journalDir, err := os.MkdirTemp("", "eqjournal-")
	if err != nil {
		t.Fatalf("Error opening temp journal dir: %v", err)
	}
	defer os.RemoveAll(journalDir)
	ctx := context.Background()
	opener := Opener(WithJournal(journalDir))

	eq1, err := entroq.New(ctx, opener)
	if err != nil {
		t.Fatalf("Error opening 1: %v", err)
	}

	if _, _, err := eq1.Modify(ctx, entroq.InsertingInto("hello", entroq.WithValue([]byte("hello")))); err != nil {
		t.Fatalf("Error inserting: %v", err)
	}

	if _, err := eq1.Claim(ctx, entroq.From("hello"), entroq.ClaimFor(0)); err != nil {
		t.Fatalf("Error claiming: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	eq1.Close()

	// Now a new client claims and deletes.
	eq2, err := entroq.New(ctx, opener)
	if err != nil {
		t.Fatalf("Error opening 2: %v", err)
	}

	task, err := eq2.Claim(ctx, entroq.From("hello"))
	if err != nil {
		t.Fatalf("Error claiming 2: %v", err)
	}

	if _, _, err := eq2.Modify(ctx, task.AsDeletion()); err != nil {
		t.Fatalf("Error deleting 2: %v", err)
	}
	eq2.Close()

	eq3, err := entroq.New(ctx, opener)
	if err != nil {
		t.Fatalf("Failed to open 3: %v", err)
	}
	eq3.Close()
}
