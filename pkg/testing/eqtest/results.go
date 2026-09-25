package eqtest

import (
	"context"
	"path"
	"slices"
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

// TasksClaimantFilter checks what a claimant filter keeps: the tasks that
// claimant can act on now, which are those anyone could claim and those it
// holds. A task's claimant is whoever last wrote it, so a future task is
// excluded unless the filtering claimant holds it, whoever wrote it, even no
// one.
func TasksClaimantFilter(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "tasks_claimant_filter")
	const me, other = "filter-me", "filter-other"
	later := time.Now().Add(time.Hour)
	insert := func(claimant string, at time.Time) string {
		t.Helper()
		resp, err := client.Modify(ctx, entroq.ModifyAs(claimant), entroq.InsertingInto(queue, entroq.WithArrivalTime(at)))
		if err != nil {
			t.Fatalf("Insert as %q: %v", claimant, err)
		}
		return resp.InsertedTasks[0].ID
	}
	want := []string{
		insert(other, time.Time{}), // available, last written by someone else
		insert(me, later),          // held by me
	}
	insert(other, later) // held by someone else
	insert("", later)    // not yet available, written by no one

	tasks, err := client.Tasks(ctx, queue, entroq.ClaimedBy(me))
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	var got []string
	for _, task := range tasks {
		got = append(got, task.ID)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("Tasks claimed by %q: got %v, want the available one and the one it holds, %v", me, got, want)
	}
}

// QueueStatsCounts checks how QueueStats divides a queue: Available is exact,
// and a task that is not yet available is Claimed if it was ever claimed and
// Future otherwise, however it came to be in the future. MaxClaims follows the
// most-claimed task still in the queue, so it falls when that task leaves.
func QueueStatsCounts(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "queue_stats_counts")
	later := time.Now().Add(time.Hour)

	resp, err := client.Modify(ctx,
		entroq.InsertingInto(queue, entroq.WithArrivalTime(later)), // future: inserted that way
		entroq.InsertingInto(queue),                                // future by a change
		entroq.InsertingInto(queue),                                // claimed three times, then held
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	delayed := resp.InsertedTasks[1]
	if _, err := client.Modify(ctx, delayed.Change(entroq.ArrivalTimeBy(time.Hour))); err != nil {
		t.Fatalf("Delay a never-claimed task: %v", err)
	}
	// The busy task is the only available one: claim it three times, then
	// hold it as a worker delaying a retry would.
	var busy *entroq.Task
	for i := range 3 {
		if busy, err = client.TryClaim(ctx, entroq.From(queue), entroq.ClaimFor(time.Minute)); err != nil || busy == nil {
			t.Fatalf("Claim %d: %v, %v", i, busy, err)
		}
		at := time.Duration(0)
		if i == 2 {
			at = time.Hour
		}
		changed, err := client.Modify(ctx, busy.Change(entroq.ArrivalTimeBy(at)))
		if err != nil {
			t.Fatalf("Release claim %d: %v", i, err)
		}
		busy = changed.ChangedTasks[0]
	}
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue)); err != nil { // available
		t.Fatalf("Insert available task: %v", err)
	}
	stat := func() *entroq.QueueStat {
		t.Helper()
		stats, err := client.QueueStats(ctx, entroq.MatchExact(queue))
		if err != nil {
			t.Fatalf("QueueStats: %v", err)
		}
		s := stats[queue]
		if s == nil {
			t.Fatalf("QueueStats: no entry for %q in %v", queue, stats)
		}
		return s
	}

	s := stat()
	if s.Size != 4 || s.Available != 1 || s.Future != 2 || s.Claimed != 1 {
		t.Errorf("QueueStats: got size %d, available %d, future %d, claimed %d; want 4, 1, 2, 1", s.Size, s.Available, s.Future, s.Claimed)
	}
	if s.MaxClaims != int(busy.Claims) {
		t.Errorf("MaxClaims: got %d, want %d", s.MaxClaims, busy.Claims)
	}

	// Repairing the task that failed most brings MaxClaims back down.
	if _, err := client.Modify(ctx, busy.Delete()); err != nil {
		t.Fatalf("Delete busy task: %v", err)
	}
	if s := stat(); s.MaxClaims >= int(busy.Claims) {
		t.Errorf("MaxClaims after the most-claimed task left: got %d, want below %d", s.MaxClaims, busy.Claims)
	}
}

// QueueStatsMatching checks that exact and prefix matches are alternatives,
// and that a limit counts only the queues reported, so emptied queues do not
// use it up.
func QueueStatsMatching(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	base := path.Join(qPrefix, "queue_stats_matching")
	exact, prefixed, emptied := path.Join(base, "exact"), path.Join(base, "pre", "a"), path.Join(base, "pre", "0-emptied")
	resp, err := client.Modify(ctx,
		entroq.InsertingInto(exact),
		entroq.InsertingInto(prefixed),
		entroq.InsertingInto(emptied),
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := client.Modify(ctx, resp.InsertedTasks[2].Delete()); err != nil {
		t.Fatalf("Empty a queue: %v", err)
	}

	stats, err := client.QueueStats(ctx, entroq.MatchExact(exact), entroq.MatchPrefix(path.Join(base, "pre")+"/"))
	if err != nil {
		t.Fatalf("QueueStats: %v", err)
	}
	if stats[exact] == nil || stats[prefixed] == nil || len(stats) != 2 {
		t.Errorf("Exact or prefix match: got %v, want %q and %q", keys(stats), exact, prefixed)
	}

	stats, err = client.QueueStats(ctx, entroq.MatchPrefix(path.Join(base, "pre")+"/"), entroq.WithLimit(1))
	if err != nil {
		t.Fatalf("QueueStats with limit: %v", err)
	}
	if stats[prefixed] == nil || len(stats) != 1 {
		t.Errorf("Limit 1 with an emptied queue matching: got %v, want %q", keys(stats), prefixed)
	}
}

// DocsOrderAndLimits checks that docs listed by key come in (key, secondary
// key, ID) order, with IDs breaking ties, so a limit cuts the same docs every
// time, and that docs looked up by ID come in the order asked for, with no
// limit applied.
func DocsOrderAndLimits(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	ns := path.Join(qPrefix, "docs_order")
	var args []entroq.ModifyArg
	for _, d := range []struct{ id, key, secondary string }{
		{"c", "k", "s"}, {"a", "k", "s"}, {"b", "k", "s"}, {"d", "k", "r"}, {"e", "l", ""},
	} {
		args = append(args, entroq.PuttingDocInto(ns, entroq.WithIDKeys(d.id, d.key, d.secondary)))
	}
	if _, err := client.Modify(ctx, args...); err != nil {
		t.Fatalf("Insert docs: %v", err)
	}
	ids := func(q *entroq.DocQuery) []string {
		t.Helper()
		q.Namespace = ns
		docs, err := client.Docs(ctx, q)
		if err != nil {
			t.Fatalf("Docs %+v: %v", q, err)
		}
		var got []string
		for _, d := range docs {
			got = append(got, d.ID)
		}
		return got
	}
	for name, test := range map[string]struct {
		q    *entroq.DocQuery
		want []string
	}{
		"all":            {&entroq.DocQuery{}, []string{"d", "a", "b", "c", "e"}},
		"range, limited": {&entroq.DocQuery{KeyStart: "k", Limit: 3}, []string{"d", "a", "b"}},
		"exact":          {&entroq.DocQuery{KeyExact: "k"}, []string{"d", "a", "b", "c"}},
		"exact, limited": {&entroq.DocQuery{KeyExact: "k", Limit: 2}, []string{"d", "a"}},
		"by ID":          {&entroq.DocQuery{IDs: []string{"e", "c", "a"}}, []string{"e", "c", "a"}},
		"by ID, limited": {&entroq.DocQuery{IDs: []string{"e", "c", "a"}, Limit: 1}, []string{"e", "c", "a"}},
	} {
		if got := ids(test.q); !slices.Equal(got, test.want) {
			t.Errorf("%s: got %v, want %v", name, got, test.want)
		}
	}
}

func keys[V any](m map[string]V) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// TaskClaimantIsHolder checks that a task's claimant names its holder: the
// writer of a task that is not yet available, and no one once it is. An
// available task with a claimant is therefore one whose claim ran out rather
// than one released on purpose.
func TaskClaimantIsHolder(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string) {
	queue := path.Join(qPrefix, "task_claimant_is_holder")
	const writer, worker = "holder-writer", "holder-worker"
	later := time.Now().Add(time.Hour)
	check := func(what string, task *entroq.Task, want string) {
		t.Helper()
		if task.Claimant != want {
			t.Errorf("%s: claimant %q, want %q", what, task.Claimant, want)
		}
		stored, err := client.Tasks(ctx, queue, entroq.WithTaskID(task.ID))
		if err != nil || len(stored) != 1 {
			t.Fatalf("%s: read back: %v, %v", what, stored, err)
		}
		if stored[0].Claimant != want {
			t.Errorf("%s: stored claimant %q, want %q", what, stored[0].Claimant, want)
		}
	}

	resp, err := client.Modify(ctx, entroq.ModifyAs(writer),
		entroq.InsertingInto(queue),
		entroq.InsertingInto(queue, entroq.WithArrivalTime(later)),
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	avail, scheduled := resp.InsertedTasks[0], resp.InsertedTasks[1]
	check("insert available", avail, "")
	check("insert for later", scheduled, writer)

	// Delaying an available task makes its modifier the holder, whatever
	// claimant the caller's copy of the task carried.
	delayed, err := client.Modify(ctx, entroq.ModifyAs(worker), avail.Change(entroq.ArrivalTimeBy(time.Hour)))
	if err != nil {
		t.Fatalf("Delay: %v", err)
	}
	check("delay", delayed.ChangedTasks[0], worker)

	released, err := client.Modify(ctx, entroq.ModifyAs(worker), delayed.ChangedTasks[0].Change(entroq.ArrivalTimeBy(0)))
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	check("release", released.ChangedTasks[0], "")

	// A claim that runs out keeps its claimant: the task is available again,
	// but nobody released it.
	claimed, err := client.TryClaim(ctx, entroq.From(queue), entroq.WithClaimant(worker), entroq.ClaimFor(50*time.Millisecond))
	if err != nil || claimed == nil {
		t.Fatalf("Claim: %v, %v", claimed, err)
	}
	check("claim", claimed, worker)
	time.Sleep(100 * time.Millisecond)
	expired, err := client.Tasks(ctx, queue, entroq.WithTaskID(claimed.ID))
	if err != nil || len(expired) != 1 {
		t.Fatalf("Read expired claim: %v, %v", expired, err)
	}
	if got := expired[0]; got.Claimant != worker || got.At.After(time.Now()) {
		t.Errorf("Expired claim: claimant %q at %v, want %q and available", got.Claimant, got.At, worker)
	}
}
