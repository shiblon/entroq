package eqtest

import (
	"context"
	"path"
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

// JournalRestoresArrivals holds a journaling backend to restoring the arrival
// times it recorded, rather than deciding them again when it replays.
//
// An arrival is an instruction while it is in flight, a duration from the
// backend's own now, and an observation once it is stored. A journal records
// what the backend DECIDED, so a replay must restore the instant: the duration
// that asked for it is not journaled, and resolving an absent one again stamps
// every replayed task with the restart time instead. That fires delayed tasks
// at once and hands out tasks that were still held, which no other test
// notices, because every assertion about an arrival is made by a process that
// never restarted.
//
// open returns a client reading the same journal each time it is called, so
// the contract can close one and reopen to force a replay. Both write paths
// are covered: a journal carries an insert as task data and a change as the
// task's final stored state, and each decides its arrival separately.
func JournalRestoresArrivals(ctx context.Context, t *testing.T, open func() (*entroq.EntroQ, error), qPrefix string) {
	t.Helper()

	client, err := open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Far enough out that a replay resolving its own now cannot be mistaken
	// for the real arrival, and that the task is still waiting when this ends.
	const delay = 24 * time.Hour
	queue := path.Join(qPrefix, "journal-arrivals")

	resp, err := client.Modify(ctx,
		entroq.InsertingInto(queue, entroq.WithArrivalTimeIn(delay), entroq.WithValue("inserted")),
		entroq.InsertingInto(queue, entroq.WithValue("to change")),
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	inserted, toChange := resp.InsertedTasks[0], resp.InsertedTasks[1]

	changeResp, err := client.Modify(ctx, toChange.Change(entroq.ArrivalTimeBy(delay)))
	if err != nil {
		t.Fatalf("Change arrival: %v", err)
	}
	changed := changeResp.ChangedTasks[0]

	want := map[string]*entroq.Task{inserted.ID: inserted, changed.ID: changed}
	for _, w := range want {
		// Without a future arrival to lose, a replay that stamps the restart
		// time would be indistinguishable from one that restores correctly.
		if !w.At.After(w.Modified) {
			t.Fatalf("task %s was written to arrive at %v, not after its modification at %v, so a stomped arrival would be undetectable here", w.ID, w.At, w.Modified)
		}
	}

	// Close and reopen on the same journal, which replays it.
	client.Close()
	if client, err = open(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer client.Close()

	got, err := client.Tasks(ctx, queue)
	if err != nil {
		t.Fatalf("Tasks after replay: %v", err)
	}
	if len(got) != len(want) {
		t.Errorf("replay restored %d tasks in %q, want %d", len(got), queue, len(want))
	}
	for _, task := range got {
		w, ok := want[task.ID]
		if !ok {
			t.Errorf("replay produced unexpected task %s", task.ID)
			continue
		}
		if !task.At.Equal(w.At) {
			t.Errorf("task %s arrival after replay: got %v, want %v as journaled (a replay that decides the arrival again stamps its own now here)", task.ID, task.At, w.At)
		}
		if !task.Created.Equal(w.Created) || !task.Modified.Equal(w.Modified) {
			t.Errorf("task %s timings after replay: got created %v modified %v, want %v and %v", task.ID, task.Created, task.Modified, w.Created, w.Modified)
		}
		// A task held when the process died keeps naming its holder until the
		// hold lapses, and the holder is decided by the arrival, so a stomped
		// arrival frees it as well as advancing it.
		if task.Claimant != w.Claimant {
			t.Errorf("task %s claimant after replay: got %q, want %q", task.ID, task.Claimant, w.Claimant)
		}
	}

	// The consequence that matters, whatever the stamps say.
	claimed, err := client.TryClaim(ctx, entroq.From(queue))
	if err != nil {
		t.Fatalf("TryClaim after replay: %v", err)
	}
	if claimed != nil {
		t.Errorf("a task arriving in %v was claimable immediately after replay: %v", delay, claimed)
	}
}
