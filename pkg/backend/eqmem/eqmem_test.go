package eqmem

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/eqmr/eqmrtest"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
)

func RunQTest(t *testing.T, tester eqtest.Tester) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "memtest-")
	if err != nil {
		t.Fatalf("Temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	ctx := context.Background()
	client, err := entroq.New(ctx, Opener(WithJournal(tmpDir)))
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer client.Close()
	tester(ctx, t, client, "")
}

func TestEQMemSimpleSequence(t *testing.T) {
	RunQTest(t, eqtest.SimpleSequence)
}

func TestEQMemTasksWithID(t *testing.T) {
	RunQTest(t, eqtest.TasksWithID)
}

func TestEQMemTasksOmitValue(t *testing.T) {
	RunQTest(t, eqtest.TasksOmitValue)
}

func TestEQMemTasksWithIDOnly(t *testing.T) {
	RunQTest(t, eqtest.TasksWithIDOnly)
}

func TestEQMemInsertWithID(t *testing.T) {
	RunQTest(t, eqtest.InsertWithID)
}

func TestEQMemSimpleChange(t *testing.T) {
	RunQTest(t, eqtest.SimpleChange)
}

func TestEQMemChangeKeepsStoredFields(t *testing.T) {
	RunQTest(t, eqtest.ChangeKeepsStoredFields)
}

func TestEQMemInsertKeepsAttemptAndErr(t *testing.T) {
	RunQTest(t, eqtest.InsertKeepsAttemptAndErr)
}

func TestEQMemModifyRejectsDuplicateIDs(t *testing.T) {
	RunQTest(t, eqtest.ModifyRejectsDuplicateIDs)
}

func TestEQMemModifyRespectsTaskClaims(t *testing.T) {
	RunQTest(t, eqtest.ModifyRespectsTaskClaims)
}

func TestEQMemTasksWithIDStaysInQueue(t *testing.T) {
	RunQTest(t, eqtest.TasksWithIDStaysInQueue)
}

func TestEQMemDocGroups(t *testing.T) {
	RunQTest(t, eqtest.DocGroups)
}

func TestEQMemTaskChangeFutureArrival(t *testing.T) {
	RunQTest(t, eqtest.TaskChangeFutureArrival)
}

func TestEQMemTaskChangeFarPastArrivalNormalized(t *testing.T) {
	RunQTest(t, eqtest.TaskChangeFarPastArrivalNormalized)
}

func TestEQMemModifyRejectsWrongQueue(t *testing.T) {
	RunQTest(t, eqtest.ModifyRejectsWrongQueue)
}

func TestEQMemEmptyWriteTargetRejected(t *testing.T) {
	RunQTest(t, eqtest.EmptyWriteTargetRejected)
}

func TestEQMemSimpleWorker(t *testing.T) {
	RunQTest(t, eqtest.SimpleWorker)
}

func TestEQMemMultiWorker(t *testing.T) {
	RunQTest(t, eqtest.MultiWorker)
}

func TestEQMemWorkerMoveOnError(t *testing.T) {
	RunQTest(t, eqtest.WorkerMoveOnError)
}

func TestEQMemWorkerRetryOnError(t *testing.T) {
	RunQTest(t, eqtest.WorkerRetryOnError)
}

func TestEQMemClaimUnblocksOnNotify(t *testing.T) {
	RunQTest(t, eqtest.ClaimUnblocksOnNotify)
}

func TestEQMemQueueMatch(t *testing.T) {
	RunQTest(t, eqtest.QueueMatch)
}

func TestEQMemQueuePrefixMatchLiteral(t *testing.T) {
	RunQTest(t, eqtest.QueuePrefixMatchLiteral)
}

func TestEQMemNamespacePrefixMatchLiteral(t *testing.T) {
	RunQTest(t, eqtest.NamespacePrefixMatchLiteral)
}

func TestEQMemQueueStats(t *testing.T) {
	RunQTest(t, eqtest.QueueStats)
}

func TestEQMemQueueStatsLimit(t *testing.T) {
	RunQTest(t, eqtest.QueueStatsLimit)
}

func TestEQMemDeleteMissingTask(t *testing.T) {
	RunQTest(t, eqtest.DeleteMissingTask)
}

func TestEQMemClaimRandomHead(t *testing.T) {
	RunQTest(t, eqtest.ClaimRandomHead)
}

func TestEQMemTasksClaimantLimit(t *testing.T) {
	RunQTest(t, eqtest.TasksClaimantLimit)
}

func TestEQMemLengthLimits(t *testing.T) {
	RunQTest(t, eqtest.LengthLimits)
}

func TestEQMemClaimLongDuration(t *testing.T) {
	RunQTest(t, eqtest.ClaimLongDuration)
}

func TestEQMemMapReduceContract(t *testing.T) {
	ctx := context.Background()
	client, err := entroq.New(ctx, Opener())
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer client.Close()
	eqtest.MapReduce(ctx, t, client, "")
}

func TestEQMemJournalMapReduceContract(t *testing.T) {
	RunQTest(t, eqtest.MapReduce)
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
		defer client.Close()
		if err := eqmrtest.QuickCheck(ctx, client, ndocs, nm, nr); err != nil {
			t.Error(err)
			return false
		}
		return true
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
		defer client.Close()
		if err := eqmrtest.QuickCheck(ctx, client, ndocs, nm, nr); err != nil {
			t.Error(err)
			return false
		}
		return true
	}
	if err := quick.Check(check, config); err != nil {
		t.Fatal(err)
	}
}

func TestEQMemMapReduce_checkHuge(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping huge MR test in short testing mode.")
	}
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
		defer client.Close()
		if err := eqmrtest.QuickCheck(ctx, client, ndocs, nm, nr); err != nil {
			t.Error(err)
			return false
		}
		return true
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
	defer eq.Close()

	if _, err := eq.Modify(ctx,
		entroq.InsertingInto("/queue/of/tasks", entroq.WithValue("hey")),
		entroq.InsertingInto("/queue/of/others", entroq.WithValue("other")),
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
	defer eq.Close()

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
	defer eq.Close()

	expectQueues := map[string]*entroq.QueueStat{
		"/queue/1": new(entroq.QueueStat),
		"/queue/2": new(entroq.QueueStat),
		"/queue/3": new(entroq.QueueStat),
	}

	// Do a bunch of random filling, keep track of how much we did.
	for q, s := range expectQueues {
		s.Name = q
		for i, n := 0, rand.Intn(maxQueueTasks); i < n; i++ {
			if _, err := eq.Modify(ctx, entroq.InsertingInto(q, entroq.WithValue(fmt.Sprintf("value %d", i)))); err != nil {
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
	defer eq.Close()

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
			if _, err := eq.Modify(ctx, task.Change(entroq.QueueTo(newQ))); err != nil {
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
	for range N {
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
	defer eq.Close()

	if _, err := eq.Modify(ctx, entroq.InsertingInto("/queue/1")); err != nil {
		t.Fatalf("Error inserting: %v", err)
	}

	task, err := eq.Claim(ctx, entroq.From("/queue/1"))
	if err != nil {
		t.Fatalf("Error claiming: %v", err)
	}

	if _, err := eq.Modify(ctx, task.Change(entroq.QueueTo(task.Queue+"/failed-parse"))); err != nil {
		t.Fatalf("Error changing: %v", err)
	}

	// /queue/2 is empty; TryClaim exercises the journal path without blocking.
	if _, err := eq.TryClaim(ctx, entroq.From("/queue/2")); err != nil {
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
	defer eq1.Close()

	if _, err := eq1.Modify(ctx, entroq.InsertingInto("hello", entroq.WithValue("hello"))); err != nil {
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
	defer eq2.Close()

	task, err := eq2.Claim(ctx, entroq.From("hello"))
	if err != nil {
		t.Fatalf("Error claiming 2: %v", err)
	}

	if _, err := eq2.Modify(ctx, task.Delete()); err != nil {
		t.Fatalf("Error deleting 2: %v", err)
	}
	eq2.Close()

	eq3, err := entroq.New(ctx, opener)
	if err != nil {
		t.Fatalf("Failed to open 3: %v", err)
	}
	defer eq3.Close()
	eq3.Close()
}

func TestEQMemDocConcurrencyStress(t *testing.T) {
	RunQTest(t, eqtest.DocConcurrencyStress)
}

func TestEQMemMixedAtomicStress(t *testing.T) {
	RunQTest(t, eqtest.MixedAtomicStress)
}

func TestEQMemWorkerCompactDependencyHandler(t *testing.T) {
	RunQTest(t, eqtest.WorkerCompactDependencyHandler)
}

func TestEQMemWorkerDependencyMove(t *testing.T) {
	RunQTest(t, eqtest.WorkerDependencyMove)
}

func TestEQMemWorkerHoldsEmptyGroup(t *testing.T) {
	RunQTest(t, eqtest.WorkerHoldsEmptyGroup)
}

func TestEQMemSimpleDocLifecycle(t *testing.T) {
	RunQTest(t, eqtest.SimpleDocLifecycle)
}

func TestEQMemInitialVersions(t *testing.T) {
	RunQTest(t, eqtest.InitialVersions)
}

func TestEQMemDocMultiOp(t *testing.T) {
	RunQTest(t, eqtest.DocMultiOp)
}

func TestEQMemDocTimestamps(t *testing.T) {
	RunQTest(t, eqtest.DocTimestamps)
}

func TestEQMemDocListing(t *testing.T) {
	RunQTest(t, eqtest.DocListing)
}

func TestEQMemDocKeyRangeByteOrder(t *testing.T) {
	RunQTest(t, eqtest.DocKeyRangeByteOrder)
}

func TestEQMemDocClaimLocking(t *testing.T) {
	RunQTest(t, eqtest.DocClaimLocking)
}

func TestEQMemDocInsertWithID(t *testing.T) {
	RunQTest(t, eqtest.DocInsertWithID)
}

func TestEQMemDocClaimantBehavior(t *testing.T) {
	RunQTest(t, eqtest.DocClaimantBehavior)
}

func TestEQMemQueueStatsAccuracy(t *testing.T) {
	RunQTest(t, eqtest.QueueStatsAccuracy)
}

func TestEQMemNamespaceStats(t *testing.T) {
	RunQTest(t, eqtest.NamespaceStats)
}

func TestEQMemJournalDocVersions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	opener := Opener(WithJournal(t.TempDir()))
	eq, err := entroq.New(ctx, opener)
	if err != nil {
		t.Fatalf("Open journaled client: %v", err)
	}

	const namespace = "/journal/doc_versions"
	resp, err := eq.Modify(ctx, entroq.PuttingDocInto(namespace,
		entroq.WithIDKeys("doc-1", "", ""),
		entroq.WithContent("initial"),
	))
	if err != nil {
		t.Fatalf("Insert doc: %v", err)
	}
	inserted := resp.InsertedDocs[0]
	if inserted.Version != 0 {
		t.Fatalf("Inserted doc version: want 0, got %d", inserted.Version)
	}

	resp, err = eq.Modify(ctx, inserted.Change(entroq.WithContent("changed")))
	if err != nil {
		t.Fatalf("Change doc: %v", err)
	}
	if got := resp.ChangedDocs[0].Version; got != 1 {
		t.Fatalf("Changed doc version: want 1, got %d", got)
	}
	if err := eq.Close(); err != nil {
		t.Fatalf("Close journaled client: %v", err)
	}

	eq, err = entroq.New(ctx, opener)
	if err != nil {
		t.Fatalf("Reopen journaled client: %v", err)
	}
	defer eq.Close()

	docs, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: namespace, IDs: []string{"doc-1"}})
	if err != nil {
		t.Fatalf("Read replayed doc: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Replayed docs length: want 1, got %d", len(docs))
	}
	if got := docs[0].Version; got != 1 {
		t.Errorf("Replayed doc version: want 1, got %d", got)
	}
}

func TestEQMemJournalReplayKeepsStoredFields(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	opener := Opener(WithJournal(t.TempDir()))
	eq, err := entroq.New(ctx, opener)
	if err != nil {
		t.Fatalf("Open journaled client: %v", err)
	}

	const queue = "/journal/stored_fields"
	if _, err := eq.Modify(ctx, entroq.InsertingInto(queue, entroq.WithValue("v0"))); err != nil {
		t.Fatalf("Insert task: %v", err)
	}
	task, err := eq.Claim(ctx, entroq.From(queue), entroq.ClaimFor(time.Minute))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	resp, err := eq.Modify(ctx, task.Change(entroq.ValueTo("v1")))
	if err != nil {
		t.Fatalf("Change task: %v", err)
	}
	wantTask := resp.ChangedTasks[0]

	const namespace = "/journal/stored_fields"
	resp, err = eq.Modify(ctx, entroq.PuttingDocInto(namespace, entroq.WithContent("v0")))
	if err != nil {
		t.Fatalf("Insert doc: %v", err)
	}
	resp, err = eq.Modify(ctx, resp.InsertedDocs[0].Change(entroq.WithContent("v1")))
	if err != nil {
		t.Fatalf("Change doc: %v", err)
	}
	wantDoc := resp.ChangedDocs[0]

	if err := eq.Close(); err != nil {
		t.Fatalf("Close journaled client: %v", err)
	}
	eq, err = entroq.New(ctx, opener)
	if err != nil {
		t.Fatalf("Reopen journaled client: %v", err)
	}
	defer eq.Close()

	tasks, err := eq.Tasks(ctx, queue, entroq.WithTaskID(wantTask.ID))
	if err != nil || len(tasks) != 1 {
		t.Fatalf("Read replayed task: %v (%d tasks)", err, len(tasks))
	}
	got := tasks[0]
	if got.Claims != 1 {
		t.Errorf("Replayed claims: want 1, got %d", got.Claims)
	}
	if !got.Created.Equal(wantTask.Created) || !got.Modified.Equal(wantTask.Modified) {
		t.Errorf("Replayed task times: want created %v, modified %v; got created %v, modified %v",
			wantTask.Created, wantTask.Modified, got.Created, got.Modified)
	}

	docs, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: namespace, IDs: []string{wantDoc.ID}})
	if err != nil || len(docs) != 1 {
		t.Fatalf("Read replayed doc: %v (%d docs)", err, len(docs))
	}
	if !docs[0].Created.Equal(wantDoc.Created) || !docs[0].Modified.Equal(wantDoc.Modified) {
		t.Errorf("Replayed doc times: want created %v, modified %v; got created %v, modified %v",
			wantDoc.Created, wantDoc.Modified, docs[0].Created, docs[0].Modified)
	}
}

// TestEQMemJournalDocClaim checks that a doc claim is journaled. The claim is
// the last thing written before a restart, so only its own journal entry can
// restore it: afterward the group is still held, and the holder can delete a
// member at the version the claim returned.
func TestEQMemJournalDocClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	opener := Opener(WithJournal(t.TempDir()))
	eq, err := entroq.New(ctx, opener)
	if err != nil {
		t.Fatalf("Open journaled client: %v", err)
	}
	const namespace = "/journal/doc_claim"
	if _, err := eq.Modify(ctx,
		entroq.PuttingDocInto(namespace, entroq.WithKeys("g", "a")),
		entroq.PuttingDocInto(namespace, entroq.WithKeys("g", "b")),
	); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	held, err := eq.ClaimDocs(ctx, entroq.ClaimKey(namespace, "g").For(time.Hour))
	if err != nil || len(held) != 2 {
		t.Fatalf("Claim: %v, %d docs", err, len(held))
	}
	holder := eq.ClientID
	if err := eq.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	eq, err = entroq.New(ctx, opener)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer eq.Close()
	if _, err := eq.ClaimDocs(ctx, &entroq.DocClaim{Namespace: namespace, Key: "g", Claimant: "other", Duration: time.Minute}); !entroq.IsDependency(err) {
		t.Errorf("Claim by another after replay: want a dependency error, got %v", err)
	}
	if _, err := eq.Modify(ctx, held[0].Delete(), entroq.ModifyAs(holder)); err != nil {
		t.Errorf("Holder delete at the claimed version after replay: %v", err)
	}
}

// TestEQMemSnapshotKeepsDocs takes a snapshot with cleanup and checks that
// reopening restores docs as well as tasks. A snapshot covers every journal
// but the live one, so each entry gets its own journal and a final unrelated
// insert keeps the docs out of the live journal: only the snapshot can bring
// them back.
func TestEQMemSnapshotKeepsDocs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	dir := t.TempDir()
	eq, err := entroq.New(ctx, Opener(WithJournal(dir), WithMaxJournalItems(1)))
	if err != nil {
		t.Fatalf("Open journaled client: %v", err)
	}
	const queue, namespace = "/snapshot/tasks", "/snapshot/docs"
	resp, err := eq.Modify(ctx,
		entroq.InsertingInto(queue, entroq.WithValue("task")),
		entroq.PuttingDocInto(namespace, entroq.WithIDKeys("doc-a", "group", "1"), entroq.WithContent("a")),
		entroq.PuttingDocInto(namespace, entroq.WithIDKeys("doc-b", "group", "2"), entroq.WithContent("b")),
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Change one doc so the snapshot has a non-initial version to restore.
	if _, err := eq.Modify(ctx, resp.InsertedDocs[0].Change(entroq.WithContent("a2"))); err != nil {
		t.Fatalf("Change doc: %v", err)
	}
	want, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: namespace})
	if err != nil {
		t.Fatalf("Read docs before snapshot: %v", err)
	}
	if _, err := eq.Modify(ctx, entroq.InsertingInto(queue+"/last")); err != nil {
		t.Fatalf("Final insert: %v", err)
	}
	if err := eq.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := TakeSnapshot(ctx, dir, true); err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}

	eq, err = entroq.New(ctx, Opener(WithJournal(dir)))
	if err != nil {
		t.Fatalf("Reopen from snapshot: %v", err)
	}
	defer eq.Close()

	got, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: namespace})
	if err != nil {
		t.Fatalf("Read docs after snapshot: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Docs after snapshot (-want +got):\n%s", diff)
	}
	tasks, err := eq.Tasks(ctx, queue)
	if err != nil {
		t.Fatalf("Read tasks after snapshot: %v", err)
	}
	if len(tasks) != 1 || string(tasks[0].Value) != `"task"` {
		t.Errorf("Tasks after snapshot: %v", tasks)
	}
}

// TestEQMemReplaysLegacyDocJournal replays doc records written before doc
// groups had locks: an insert, then a change recorded at v2, the final version
// eqmem gave a doc's first change before v1.12.1 started docs at v0. Replay
// applies the recorded state without checking versions, and gives the group a
// lock one version past its highest member, so any version read from the old
// journal is stale.
func TestEQMemReplaysLegacyDocJournal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	opener := WithJournal(t.TempDir())
	m, err := New(ctx, opener)
	if err != nil {
		t.Fatalf("Open journaled backend: %v", err)
	}

	const namespace = "/journal/legacy_doc_version"
	legacyInsert := &entroq.DocData{Namespace: namespace, ID: "doc-1", Content: json.RawMessage(`"initial"`)}
	legacyChange := &entroq.Doc{Namespace: namespace, ID: "doc-1", Version: 2, Content: json.RawMessage(`"changed"`)}
	for _, mod := range []*entroq.Modification{
		{DocInserts: []*entroq.DocData{legacyInsert}},
		{DocChanges: []*entroq.Doc{legacyChange}},
	} {
		record, err := json.Marshal(mod)
		if err != nil {
			t.Fatalf("Marshal legacy journal record: %v", err)
		}
		if err := m.journal.Append(record); err != nil {
			t.Fatalf("Append legacy journal record: %v", err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close journaled backend: %v", err)
	}

	m, err = New(ctx, opener)
	if err != nil {
		t.Fatalf("Replay legacy journal: %v", err)
	}
	defer m.Close()

	docs, err := m.Docs(ctx, &entroq.DocQuery{Namespace: namespace, IDs: []string{"doc-1"}})
	if err != nil {
		t.Fatalf("Read replayed doc: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Replayed docs length: want 1, got %d", len(docs))
	}
	if got := string(docs[0].Content); got != `"changed"` {
		t.Errorf("Replayed legacy doc content: want %q, got %q", `"changed"`, got)
	}
	if got := docs[0].Version; got != 3 {
		t.Errorf("Replayed legacy group version: want 3 (one past the highest member), got %d", got)
	}

	// The group is writable at its new version.
	if _, err := m.Modify(ctx, entroq.NewModification("", docs[0].Change(entroq.WithContent("after")))); err != nil {
		t.Errorf("Change after replay: %v", err)
	}
}

// TestEQMemReplayBackfillsMissingQueue covers the journal read path for older
// journals: a change recorded before the queue-as-modify-key requirement carries
// its queue but an empty FromQueue. Such an op must be rejected on the live write
// path (the queue is the modify key and an external request must name it) yet
// backfilled from stored state and applied on trusted replay, so old journals
// still replay cleanly.
func TestEQMemReplayBackfillsMissingQueue(t *testing.T) {
	ctx := context.Background()
	m, err := New(ctx)
	if err != nil {
		t.Fatalf("new eqmem: %v", err)
	}
	defer m.Close()

	const q = "/replay/backfill"
	// The backend Modify does not generate IDs (the entroq client does), so supply one.
	ins, err := m.Modify(ctx, entroq.NewModification("", entroq.InsertingInto(q, entroq.WithID("backfill-task"), entroq.WithValue("x"))))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	task := ins.InsertedTasks[0]

	// A change as an older journal recorded it: queue present, FromQueue empty.
	// Rebuilt per call because a successful apply advances the stored version.
	oldStyle := func() *entroq.Modification {
		return &entroq.Modification{
			Changes: []*entroq.Task{{ID: task.ID, Version: task.Version, Queue: q, Value: json.RawMessage(`"y"`)}},
		}
	}

	// Live (non-replay) rejects the empty FromQueue.
	if _, err := m.modifyImpl(ctx, oldStyle(), false); err == nil {
		t.Error("live change with empty FromQueue should be rejected, got nil error")
	}

	// Replay backfills FromQueue from stored state (qByID) and applies the change.
	if _, err := m.modifyImpl(ctx, oldStyle(), true); err != nil {
		t.Fatalf("replay of a queue-less change should backfill and succeed: %v", err)
	}

	// Proof it applied: replaying the same (now stale) version fails the version check.
	if _, err := m.modifyImpl(ctx, oldStyle(), true); err == nil {
		t.Error("second replay at the stale version should fail the version check, got nil error")
	}
}

func TestEQMemModifyReportsAllFailureClasses(t *testing.T) {
	RunQTest(t, eqtest.ModifyReportsAllFailureClasses)
}

func TestEQMemModifyRejectsWrongNamespace(t *testing.T) {
	RunQTest(t, eqtest.ModifyRejectsWrongNamespace)
}
