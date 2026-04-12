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
	changed[0].Queue = inQueue

	if diff := EqualTasksVersionIncr(inserted[0], changed[0], 1); diff != "" {
		t.Fatalf("Tasks not equal (except version bump):\n%v", diff)
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

// WorkerDependencyHandler tests that the worker calls the dependency handler when a finish modify fails.
