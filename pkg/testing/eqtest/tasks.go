package eqtest

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/shiblon/entroq"
)

func SimpleChange(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	inQueue := path.Join(qPrefix, "simple_change", "in")
	outQueue := path.Join(qPrefix, "simple_change", "out")

	resp, err := client.Modify(ctx, entroq.InsertingInto(inQueue))
	if err != nil {
		t.Fatalf("Error inserting: %v", err)
	}
	inserted := resp.InsertedTasks
	resp, err = client.Modify(ctx, inserted[0].Change(entroq.QueueTo(outQueue)))
	if err != nil {
		t.Fatalf("Error changing: %v", err)
	}
	changed := resp.ChangedTasks
	if changed[0].Queue != outQueue {
		t.Fatalf("Change queue: want %q, got %v", outQueue, changed[0].Queue)
	}
	// Modifying a task clears the claimant -- it is no longer held.
	if changed[0].Claimant != "" {
		t.Fatalf("Expected claimant to be cleared after change, got %q", changed[0].Claimant)
	}
	changed[0].Queue = inQueue

	// Clone inserted[0] with claimant zeroed so the version-bump comparison is apples-to-apples.
	wantBase := *inserted[0]
	wantBase.Claimant = ""
	if diff := EqualTasksVersionIncr(&wantBase, changed[0], 1); diff != "" {
		t.Fatalf("Tasks not equal (except version bump and claimant):\n%v", diff)
	}
}

func TaskChangeFutureArrival(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "task_change_future_arrival")
	const delay = time.Second

	resp, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithValue("later")))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	inserted := resp.InsertedTasks[0]

	before, err := client.Time(ctx)
	if err != nil {
		t.Fatalf("time before change: %v", err)
	}
	resp, err = client.Modify(ctx, inserted.Change(entroq.ArrivalTimeBy(delay)))
	if err != nil {
		t.Fatalf("change arrival: %v", err)
	}
	changed := resp.ChangedTasks[0]
	if changed.At.Before(before.Add(delay - 100*time.Millisecond)) {
		t.Fatalf("changed arrival time is too early: got %s, want about %s", changed.At.Format(time.RFC3339Nano), before.Add(delay).Format(time.RFC3339Nano))
	}

	tasks, err := client.Tasks(ctx, queue)
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks: got %d, want 1", len(tasks))
	}
	if tasks[0].At.Before(before.Add(delay - 100*time.Millisecond)) {
		t.Fatalf("stored arrival time is too early: got %s, want about %s", tasks[0].At.Format(time.RFC3339Nano), before.Add(delay).Format(time.RFC3339Nano))
	}

	claimed, err := client.TryClaim(ctx, entroq.From(queue), entroq.ClaimFor(100*time.Millisecond))
	if err != nil {
		t.Fatalf("try claim future task: %v", err)
	}
	if claimed != nil {
		t.Fatalf("future task was claimable immediately: %s", claimed)
	}
}

// SimpleWorker tests basic worker functionality while tasks are coming in and
// being waited on.
func ClaimUnblocksOnNotify(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	t.Helper()
	queue := path.Join(qPrefix, "notify_unblock")

	// Long poll so the test fails obviously if notification doesn't fire.
	const pollInterval = 1 * time.Minute

	claimCh := make(chan *entroq.Task, 1)
	ready := make(chan struct{})

	go func() {
		close(ready)
		task, err := client.Claim(ctx, entroq.From(queue), entroq.ClaimPollTime(pollInterval))
		if err != nil {
			t.Errorf("claim error: %v", err)
			return
		}
		claimCh <- task
	}()

	<-ready
	time.Sleep(300 * time.Millisecond) // let Claim reach its wait before inserting

	start := time.Now()
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithValue("ping"))); err != nil {
		t.Fatalf("insert: %v", err)
	}

	select {
	case <-claimCh:
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("Claim took %v after insert -- notification may not have fired (poll interval was %v)",
				elapsed, pollInterval)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Claim did not unblock within 10s after insert")
	}
}

// TasksOmitValue exercises the task query where values are not desired.
func TasksOmitValue(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "tasks_omit_value")

	resp, err := client.Modify(ctx,
		entroq.InsertingInto(queue, entroq.WithValue("t1")),
		entroq.InsertingInto(queue, entroq.WithValue("t2")),
		entroq.InsertingInto(queue, entroq.WithValue("t3")),
	)
	if err != nil {
		t.Fatalf("Failed to insert tasks: %v", err)
	}
	inserted := resp.InsertedTasks

	tasks, err := client.Tasks(ctx, queue, entroq.OmitValues())
	if err != nil {
		t.Fatalf("Failed to get tasks without values: %v", err)
	}
	if diff := EqualAllTasksUnorderedSkipTimesAndCounters(inserted, tasks, expectEmptyValue()); diff != "" {
		t.Errorf("Task listing without values had unexpected results (-want +got):\n%v", diff)
	}

	tasksWithVals, err := client.Tasks(ctx, queue)
	if err != nil {
		t.Fatalf("Failed to get tasks with values: %v", err)
	}
	if diff := EqualAllTasksUnorderedSkipTimesAndCounters(inserted, tasksWithVals); diff != "" {
		t.Fatalf("Task listing with values had unexpected results (-want +got):\n%v", diff)
	}
}

// TasksWithID exercises the task query mechanism that allows specific task IDs to be looked up.
func TasksWithID(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "tasks_with_id")

	ids := []string{
		client.GenID(),
		client.GenID(),
		client.GenID(),
		client.GenID(),
	}

	var args []entroq.ModifyArg
	for _, id := range ids {
		args = append(args, entroq.InsertingInto(queue, entroq.WithID(id)))
	}
	resp, err := client.Modify(ctx, args...)
	if err != nil {
		t.Fatalf("Insertion failed: %v", err)
	}
	inserted := resp.InsertedTasks
	if want, got := len(ids), len(inserted); want != got {
		t.Fatalf("Expected %d tasks inserted, got %d", want, got)
	}
	for i, task := range inserted {
		if want, got := ids[i], task.ID; want != got {
			t.Fatalf("Inserted task should have ID %q, but has %q", want, got)
		}
	}

	// Once inserted, we should be able to query for zero (all), one, or more of them.

	// Check that no ID spec produces the right number of them.
	tasks, err := client.Tasks(ctx, queue)
	if err != nil {
		t.Fatalf("Error getting tasks from queue %q: %v", queue, err)
	}
	if want, got := len(ids), len(tasks); want != got {
		t.Fatalf("Expected %d tasks in 'all' query, got %d", want, got)
	}
	want := make(map[string]bool)
	for _, id := range ids {
		want[id] = true
	}
	for _, task := range tasks {
		if !want[task.ID] {
			t.Fatalf("Wanted queried task %s to have ID present in task listing, but not found", task.ID)
		}
	}

	// Check that specifing a couple of the IDs works.
	idSubSet := []string{ids[1], ids[3]}
	tasks, err = client.Tasks(ctx, queue, entroq.WithTaskID(idSubSet...))
	if err != nil {
		t.Fatalf("Error getting tasks from queue %q: %v", queue, err)
	}
	if want, got := len(idSubSet), len(tasks); want != got {
		t.Fatalf("Expected %d tasks in 'all' query, got %d", want, got)
	}
	for i, task := range tasks {
		if want, got := idSubSet[i], task.ID; want != got {
			t.Fatalf("Wanted queried task %d to have ID %q, got %q", i, want, got)
		}
	}
}

// TasksClaimantLimit pins down the Tasks(Queue, Claimant, Limit) contract, the
// one no other test covered. A claimant-filtered listing returns tasks that are
// available/expired (arrival time in the past) OR claimed by that claimant, and
// Limit caps the count of MATCHING tasks -- min(Limit, #matching) -- not the
// number of candidates inspected. The distinction matters: a backend that
// applies Limit before filtering can return fewer than that, even zero, while
// matching tasks exist. This exercises exactly that trap.
func TasksClaimantLimit(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	t.Helper()

	const (
		me    = "worker-me"
		other = "worker-other"
	)

	idSet := func(tasks []*entroq.Task) map[string]bool {
		s := make(map[string]bool, len(tasks))
		for _, task := range tasks {
			s[task.ID] = true
		}
		return s
	}

	// Case 1: with only available tasks, Limit caps the count and every result
	// matches (available tasks always satisfy a claimant filter).
	t.Run("available_limit", func(t *testing.T) {
		queue := path.Join(qPrefix, "claimant_limit", "available")
		past := time.Now().Add(-time.Hour).UTC()
		var args []entroq.ModifyArg
		for i := 0; i < 10; i++ {
			args = append(args, entroq.InsertingInto(queue, entroq.WithArrivalTime(past)))
		}
		if _, err := client.Modify(ctx, args...); err != nil {
			t.Fatalf("insert available: %v", err)
		}

		tasks, err := client.Tasks(ctx, queue, entroq.ClaimedBy(me), entroq.LimitTasks(4))
		if err != nil {
			t.Fatalf("tasks: %v", err)
		}
		if len(tasks) != 4 {
			t.Fatalf("available+limit: want 4 tasks, got %d", len(tasks))
		}

		// With Limit above the matching count, all 10 come back.
		tasks, err = client.Tasks(ctx, queue, entroq.ClaimedBy(me), entroq.LimitTasks(50))
		if err != nil {
			t.Fatalf("tasks (high limit): %v", err)
		}
		if len(tasks) != 10 {
			t.Fatalf("available+high limit: want 10 tasks, got %d", len(tasks))
		}
	})

	// Case 2: the zero-results trap. Fill a queue so that the lowest-arrival
	// tasks are all claimed by SOMEONE ELSE (nearer expiry), while the querying
	// claimant's own tasks sit behind them (farther expiry). A backend that
	// limits candidates before filtering scans only the other-owned wall and
	// returns nothing; the contract requires min(Limit, #mine) of the querier's
	// own tasks.
	t.Run("mine_behind_others", func(t *testing.T) {
		queue := path.Join(qPrefix, "claimant_limit", "behind")
		const (
			nMine      = 5
			nOther     = 10
			myClaim    = 20 * time.Second // farther expiry: sorts AFTER others
			otherClaim = 10 * time.Second // nearer expiry: sorts ahead of mine
		)

		past := time.Now().Add(-time.Hour).UTC()
		var args []entroq.ModifyArg
		for i := 0; i < nMine+nOther; i++ {
			args = append(args, entroq.InsertingInto(queue, entroq.WithArrivalTime(past)))
		}
		if _, err := client.Modify(ctx, args...); err != nil {
			t.Fatalf("insert pool: %v", err)
		}

		// Claim nMine as the querying claimant with the farther expiry.
		mine := make(map[string]bool, nMine)
		for i := 0; i < nMine; i++ {
			task, err := client.TryClaim(ctx, entroq.From(queue), entroq.WithClaimant(me), entroq.ClaimFor(myClaim))
			if err != nil {
				t.Fatalf("claim mine %d: %v", i, err)
			}
			if task == nil {
				t.Fatalf("claim mine %d: nothing available", i)
			}
			mine[task.ID] = true
		}
		// Claim the rest as another claimant with a nearer expiry, so they sort
		// ahead of "mine" by arrival time.
		for i := 0; i < nOther; i++ {
			task, err := client.TryClaim(ctx, entroq.From(queue), entroq.WithClaimant(other), entroq.ClaimFor(otherClaim))
			if err != nil {
				t.Fatalf("claim other %d: %v", i, err)
			}
			if task == nil {
				t.Fatalf("claim other %d: nothing available", i)
			}
		}

		// Limit below nMine: expect exactly Limit, all owned by me.
		tasks, err := client.Tasks(ctx, queue, entroq.ClaimedBy(me), entroq.LimitTasks(3))
		if err != nil {
			t.Fatalf("tasks limited: %v", err)
		}
		if len(tasks) != 3 {
			t.Fatalf("mine-behind-others with Limit=3: want 3, got %d (a pre-filter limit returns 0 here)", len(tasks))
		}
		for _, task := range tasks {
			if !mine[task.ID] {
				t.Fatalf("returned task %s is not owned by %q", task.ID, me)
			}
		}

		// Limit above nMine: expect all nMine of my tasks, and only mine.
		tasks, err = client.Tasks(ctx, queue, entroq.ClaimedBy(me), entroq.LimitTasks(50))
		if err != nil {
			t.Fatalf("tasks high limit: %v", err)
		}
		got := idSet(tasks)
		if len(got) != nMine {
			t.Fatalf("mine-behind-others with high Limit: want %d, got %d", nMine, len(got))
		}
		for id := range mine {
			if !got[id] {
				t.Fatalf("expected my task %s in results, missing", id)
			}
		}
	})
}

// TasksWithIDOnly tests that tasks listed by ID only (no queue) can return from multiple queues.
func TasksWithIDOnly(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	q1 := path.Join(qPrefix, "id_only_1")
	q2 := path.Join(qPrefix, "id_only_2")

	var modArgs []entroq.ModifyArg
	for i := 0; i < 5; i++ {
		q := q1
		if i%2 == 0 {
			q = q2
		}
		modArgs = append(modArgs, entroq.InsertingInto(q, entroq.WithValue(fmt.Sprintf("val %d", i))))
	}

	resp, err := client.Modify(ctx, modArgs...)
	if err != nil {
		t.Fatalf("Initial insert failed: %v", err)
	}
	ins := resp.InsertedTasks

	var ids1, ids2 []string
	var tasks1, tasks2 []*entroq.Task
	for i, t := range ins {
		if i < 3 {
			tasks1 = append(tasks1, t)
			ids1 = append(ids1, t.ID)
		} else {
			tasks2 = append(tasks2, t)
			ids2 = append(ids2, t.ID)
		}
	}

	results1, err := client.Tasks(ctx, "", entroq.WithTaskID(ids1...))
	if err != nil {
		t.Errorf("First group of task IDs had an error: %v", err)
	}
	for i, task := range results1 {
		if want, got := ids1[i], task.ID; want != got {
			t.Errorf("Expected task %d from group 1 to have ID %v, got %v", i, want, got)
		}
		if want, got := string(tasks1[i].Value), string(task.Value); want != got {
			t.Errorf("Expected task %d from group 1 to have bytes %s, got %s", i, want, got)
		}
	}

	results2, err := client.Tasks(ctx, "", entroq.WithTaskID(ids2...))
	if err != nil {
		t.Errorf("First group of task IDs had an error: %v", err)
	}
	for i, task := range results2 {
		if want, got := ids2[i], task.ID; want != got {
			t.Errorf("Expected task %d from group 2 to have ID %v, got %v", i, want, got)
		}
		if want, got := string(tasks2[i].Value), string(task.Value); want != got {
			t.Errorf("Expected task %d from group 2 to have bytes %s, got %s", i, want, got)
		}
	}
}

// InsertWithID tests the ability to insert tasks with a specified ID,
// including errors when an existing ID is used for insertion.
func InsertWithID(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "insert_with_id")

	knownID := client.GenID()

	// Insert task with an explicit ID.
	resp, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithID(knownID)))
	if err != nil {
		t.Fatalf("Unable to insert task with known ID %q: %v", knownID, err)
	}
	inserted := resp.InsertedTasks
	changed := resp.ChangedTasks

	// Check that insertion with explicit IDs works.
	if len(changed) != 0 {
		t.Fatalf("Expected 0 changed tasks, got %d", len(changed))
	}
	if len(inserted) != 1 {
		t.Fatalf("Expected 1 inserted task, got %d", len(inserted))
	}

	insertedTask := inserted[0]
	if insertedTask.ID != knownID {
		t.Fatalf("Expected inserted task to have ID %q, got %q", knownID, insertedTask.ID)
	}

	// Try to claim the just-inserted task.
	claimed, err := client.TryClaim(ctx, entroq.From(queue), entroq.ClaimFor(100*time.Millisecond))
	if err != nil {
		t.Fatalf("Unexpected error claiming task with ID %q: %v", knownID, err)
	}
	if claimed == nil {
		t.Fatalf("Expected task from queue %q, but received none", queue)
	}
	if claimed.ID != knownID {
		t.Fatalf("Task claim expected ID %q, got %q", knownID, claimed.ID)
	}

	// Try to insert with a known ID that's already there.
	_, err = client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithID(knownID)))
	if err == nil {
		t.Fatalf("Expected error inserting with existing ID %v, but got no error", knownID)
	}
	depErr, ok := entroq.AsDependency(err)
	if !ok {
		t.Fatalf("Expected dependency error, got %v", err)
	}
	if want, got := 1, len(depErr.Inserts); want != got {
		t.Fatalf("Expected %d insertion errors in dependency error, got %v", want, got)
	}

	// Try to insert with a known ID when the task is in a different queue.
	_, err = client.Modify(ctx, entroq.InsertingInto(queue+"/elsewhere", entroq.WithID(knownID)))
	if err == nil {
		t.Fatalf("Expected error inserting existing ID %v into a different queue, but got no error", knownID)
	}
	depErr, ok = entroq.AsDependency(err)
	if !ok {
		t.Fatalf("Expected dependency error, got %v", err)
	}
	if want, got := 1, len(depErr.Inserts); want != got {
		t.Fatalf("Expected %d insertion errors in dependency error, got %v", want, got)
	}

	// Try to insert again, but allow it to be skipped.
	_, err = client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithID(knownID), entroq.WithSkipColliding(true)))
	if err != nil {
		t.Fatalf("Expected no error inserting with existing skippable ID %v: %v", knownID, err)
	}

	resp, err = client.Modify(ctx, entroq.InsertingInto(queue))
	if err != nil {
		t.Fatalf("Expected no insertion error, got: %v", err)
	}
	inserted = resp.InsertedTasks

	// Try to insert the known ID and delete the new ID at the same time. This
	// should work when it's set to skip colliding.
	if _, err = client.Modify(ctx,
		entroq.InsertingInto(queue,
			entroq.WithID(knownID),
			entroq.WithSkipColliding(true)),
		inserted[0].Delete()); err != nil {
		t.Fatalf("Expected no error inserting skippable and deleting, got: %v", err)
	}

	// Check that we have only one task in the queue, and that it's the expected one.
	tasks, err := client.Tasks(ctx, queue)
	if err != nil {
		t.Fatalf("Error getting tasks: %v", err)
	}
	if want, got := 1, len(tasks); want != got {
		t.Fatalf("Expected len(tasks) = %d, got %v", want, got)
	}
	if want, got := knownID, tasks[0].ID; want != got {
		t.Fatalf("Expected ID %v found, got %v", want, got)
	}
}

// SimpleSequence tests some basic functionality of a task manager, over gRPC.
func SimpleSequence(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	now, err := client.Time(ctx)
	if err != nil {
		t.Fatalf("Failed to get backend time: %v", err)
	}

	queue := path.Join(qPrefix, "simple_sequence")

	// Claim from empty queue.
	task, err := client.TryClaim(ctx, entroq.From(queue), entroq.ClaimFor(100*time.Millisecond))
	if err != nil {
		t.Fatalf("Got unexpected error claiming from empty queue: %v", err)
	}
	if task != nil {
		t.Fatalf("Got unexpected non-nil claim response from empty queue:\n%s", task)
	}

	const futureTaskDuration = 2 * time.Second
	futureTime := now.Add(futureTaskDuration)

	helloVal, _ := json.Marshal("hello")
	thereVal, _ := json.Marshal("there")
	insWant := []*entroq.Task{
		{
			Queue:    queue,
			At:       now,
			Value:    helloVal,
			Claimant: client.ID(),
		},
		{
			Queue:    queue,
			At:       futureTime,
			Value:    thereVal,
			Claimant: client.ID(),
		},
	}
	var insData []*entroq.TaskData
	for _, task := range insWant {
		insData = append(insData, task.Data())
	}

	resp, err := client.Modify(ctx, entroq.Inserting(insData...))
	if err != nil {
		t.Fatalf("Got unexpected error inserting two tasks: %+v", err)
	}
	inserted := resp.InsertedTasks
	changed := resp.ChangedTasks
	if changed != nil {
		t.Fatalf("Got unexpected changes during insertion: %+v", err)
	}
	if diff := EqualAllTasksOrderedSkipIDAndTime(insWant, inserted); diff != "" {
		t.Fatalf("Modify tasks unexpected result, ignoring ID and time fields (-want +got):\n%v", diff)
	}
	// Also check that their arrival times are 100 ms apart as expected:
	if diff := inserted[1].At.Sub(inserted[0].At); diff != futureTaskDuration {
		t.Fatalf("Wanted At difference to be %v, got %v", futureTaskDuration, diff)
	}

	// Get queues.
	queuesWant := map[string]int{queue: 2}
	queuesGot, err := client.Queues(ctx, entroq.MatchPrefix(qPrefix))
	if err != nil {
		t.Fatalf("Getting queues failed: %v", err)
	}
	if diff := cmp.Diff(queuesWant, queuesGot); diff != "" {
		t.Fatalf("Queues (-want +got):\n%v", diff)
	}

	// Get all tasks.
	tasksGot, err := client.Tasks(ctx, queue)
	if err != nil {
		t.Fatalf("Tasks call failed after insertions: %v", err)
	}
	if diff := EqualAllTasksUnorderedSkipTimesAndCounters(inserted, tasksGot); diff != "" {
		t.Fatalf("Tasks unexpected return, ignoring ID and time fields (-want +got):\n%+v", diff)
	}

	// Claim ready task.
	claimCtx, claimCancel := context.WithTimeout(ctx, 5*time.Second)
	claimed, err := client.Claim(claimCtx, entroq.From(queue), entroq.ClaimFor(10*time.Second))
	claimCancel()

	if err != nil {
		t.Fatalf("Got unexpected error for claiming from a queue with one ready task: %+v", err)
	}
	if claimed == nil {
		t.Fatalf("Unexpected nil result from blocking Claim")
	}
	if diff := EqualTasksVersionIncr(inserted[0], claimed, 1); diff != "" {
		t.Fatalf("Claim tasks differ, ignoring ID and times:\n%v", diff)
	}
	if claimed.Claims != 1 {
		t.Fatalf("Expected claim to increment task claims to %d, got %d", 1, claimed.Claims)
	}

	// TryClaim not ready task.
	tryclaimed, err := client.TryClaim(ctx, entroq.From(queue), entroq.ClaimFor(10*time.Second))
	if err != nil {
		t.Fatalf("Got unexpected error for claiming from a queue with no ready tasks: %v", err)
	}
	if tryclaimed != nil {
		t.Fatalf("Got unexpected non-nil claim response from a queue with no ready tasks:\n%s", tryclaimed)
	}

	// Make sure the next claim will work.
	claimCtx, cancel := context.WithTimeout(ctx, 2*futureTaskDuration)
	defer cancel()
	claimed, err = client.Claim(claimCtx,
		entroq.From(queue),
		entroq.ClaimFor(5*time.Second),
		entroq.ClaimPollTime(time.Second))
	if err != nil {
		t.Fatalf("Got unexpected error for claiming from a queue with one ready task: %v", err)
	}
	if diff := EqualTasksVersionIncr(inserted[1], claimed, 1); diff != "" {
		t.Fatalf("Claim got unexpected task, ignoring ID and time fields (-want +got):\n%v", diff)
	}
	log.Printf("Now: %v", now)
	if got := claimed.At; got.Before(futureTime) {
		t.Fatalf("Claimed arrival time %v came earlier than expedcted time %v", got, futureTime)
	}
	if claimed.Claims != 1 {
		t.Fatalf("Expected claim to increment task claims to %d, got %d", 1, claimed.Claims)
	}
}

// QueueMatch tests various queue matching functions against a client.
func DeleteMissingTask(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	//queue := path.Join(qPrefix, "delete_missing")

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if _, err := client.Modify(ctx, entroq.Deleting("fake_task_id", 0)); err != nil {
		if depErr, ok := entroq.AsDependency(err); !ok {
			t.Fatalf("Expected dependency error when deleting missing task, got: %v", err)
		} else {
			if want, got := 1, len(depErr.Deletes); want != got {
				t.Fatalf("Expected 1 delete, got: %v", got)
			}
		}
	} else {
		t.Fatalf("Expected error when deleting missing task, got: %v", err)
	}

	resp, err := client.Modify(ctx, entroq.InsertingInto("my queue", entroq.WithValue("hi")))
	if err != nil {
		t.Fatalf("Error inserting task for delete missing test: %v", err)
	}
	ins := resp.InsertedTasks

	if _, err := client.Modify(ctx, entroq.Deleting(ins[0].ID, 1 /* wrong version */)); err != nil {
		if depErr, ok := entroq.AsDependency(err); !ok {
			t.Fatalf("Expected dependency error when deleting with wrong version, got: %v", err)
		} else {
			if want, got := 1, len(depErr.Deletes); want != got {
				t.Fatalf("Expected 1 delete, got: %v", got)
			}
		}
	} else {
		t.Fatalf("Expected error when deleting with wrong version, got: %v", err)
	}
}

// ClaimRandomHead verifies EntroQ's anti-starvation contract: when several
// tasks are equally eligible (same arrival time, all available), TryClaim must
// NOT deterministically pick the same one every time. A backend that always
// returns, say, the lowest-ID task would let a persistently-failing task
// re-claim forever and starve the rest of the queue -- so random selection
// among the most-overdue available tasks is part of the interface, not an
// implementation detail. Every backend must satisfy it (eqmem via probabilistic
// heap descent, eqpg via ID-hash buckets, eqredis via a random window offset).
//
// The test runs many independent trials. Each trial uses a FRESH queue and two
// freshly-inserted tasks with server-assigned IDs and an identical arrival time.
// Fresh IDs per trial matter: a backend that spreads by hashing the ID (eqpg)
// would legitimately favor one member of a FIXED pair whose hashes happen to
// collide in the same bucket, so reusing IDs would make a correct backend look
// broken. With new IDs each trial, any per-pair skew averages out.
//
// We track two labelings of the winner, because a determinism bug could latch
// onto either: the lexically-smaller ID (the shape of the eqredis regression
// this test was written to catch) and the first-inserted task (a created-time
// tiebreak, say). A fair backend lands each near 50%; the band below is loose
// enough that no fair backend flakes (eqmem's heap skews one metric to ~2/3,
// still well inside) yet a fully deterministic backend (0% or 100%) always
// trips it. The band is two-sided on purpose: "smaller ID wins < X%" alone would
// silently pass a backend that ALWAYS picks the larger ID.
func ClaimRandomHead(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	t.Helper()

	const (
		trials  = 300
		minFrac = 0.10 // deterministic-one-side => 0.0; fair (incl. eqmem ~0.33) stays well above
		maxFrac = 0.90 // deterministic-other-side => 1.0; fair (incl. eqmem ~0.67) stays well below
	)

	// A fixed arrival time in the past: both tasks are immediately claimable and
	// perfectly tied, so nothing but the backend's tiebreak decides the winner.
	at := time.Now().Add(-time.Hour).UTC()

	var smallerIDWins, firstInsertedWins int
	for i := 0; i < trials; i++ {
		queue := path.Join(qPrefix, "claim_random_head", fmt.Sprint(i))

		resp, err := client.Modify(ctx,
			entroq.InsertingInto(queue, entroq.WithValue("a"), entroq.WithArrivalTime(at)),
			entroq.InsertingInto(queue, entroq.WithValue("b"), entroq.WithArrivalTime(at)),
		)
		if err != nil {
			t.Fatalf("trial %d: insert pair: %v", i, err)
		}
		if len(resp.InsertedTasks) != 2 {
			t.Fatalf("trial %d: want 2 inserted, got %d", i, len(resp.InsertedTasks))
		}
		first := resp.InsertedTasks[0].ID
		smaller := resp.InsertedTasks[0].ID
		if resp.InsertedTasks[1].ID < smaller {
			smaller = resp.InsertedTasks[1].ID
		}

		claimed, err := client.TryClaim(ctx, entroq.From(queue), entroq.ClaimFor(time.Minute))
		if err != nil {
			t.Fatalf("trial %d: try claim: %v", i, err)
		}
		if claimed == nil {
			t.Fatalf("trial %d: nothing claimed from a queue with two available tasks", i)
		}

		if claimed.ID == smaller {
			smallerIDWins++
		}
		if claimed.ID == first {
			firstInsertedWins++
		}
	}

	smallerFrac := float64(smallerIDWins) / trials
	firstFrac := float64(firstInsertedWins) / trials
	t.Logf("over %d trials: smaller-ID won %.1f%%, first-inserted won %.1f%% (each should sit near 50%%)",
		trials, smallerFrac*100, firstFrac*100)

	if smallerFrac < minFrac || smallerFrac > maxFrac {
		t.Errorf("claim head selection looks deterministic by ID: smaller-ID won %.1f%% of %d trials, want within [%.0f%%, %.0f%%]",
			smallerFrac*100, trials, minFrac*100, maxFrac*100)
	}
	if firstFrac < minFrac || firstFrac > maxFrac {
		t.Errorf("claim head selection looks deterministic by insertion order: first-inserted won %.1f%% of %d trials, want within [%.0f%%, %.0f%%]",
			firstFrac*100, trials, minFrac*100, maxFrac*100)
	}
}

// WorkerDependencyHandler tests that the worker calls the dependency handler when a finish modify fails.
