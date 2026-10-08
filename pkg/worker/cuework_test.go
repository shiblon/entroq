package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// cueFixture stands up a worker over a shared in-memory backend with one task
// in queue "q", and hands back an intruder that shares the store but not the
// claimant, so a test can ask whether the task is really free.
type cueFixture struct {
	client   *entroq.EntroQ
	intruder *entroq.EntroQ
}

func newCueFixture(ctx context.Context, t *testing.T) *cueFixture {
	t.Helper()
	backend, err := eqmem.Opener()(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	shared := func(context.Context) (entroq.Backend, error) { return backend, nil }
	client, err := entroq.New(ctx, shared)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	intruder, err := entroq.New(ctx, shared, entroq.WithClaimantID("intruder"))
	if err != nil {
		t.Fatalf("New intruder: %v", err)
	}
	if _, err := client.Modify(ctx, entroq.InsertingInto("q")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	return &cueFixture{client: client, intruder: intruder}
}

// TestCueWorkRunsBeforeRenewalWithTheSets checks the phase's position: the cue
// sees the doc sets already held, and it runs before DoWork.
func TestCueWorkRunsBeforeRenewalWithTheSets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := newCueFixture(ctx, t)

	type observation struct {
		sets  int
		order []string
	}
	var (
		mu  sync.Mutex
		obs observation
	)
	note := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		obs.order = append(obs.order, what)
	}

	done := make(chan struct{})
	w := New[string](f.client,
		WithTakeDocs(func(context.Context, entroq.Reader, *Work[string]) (*TakeResult, error) {
			note("take")
			return Take(entroq.ClaimKey("ns", "k")), nil
		}),
		WithCueWork(func(_ context.Context, _ entroq.Reader, work *Work[string]) error {
			note("cue")
			mu.Lock()
			obs.sets = len(work.Sets)
			mu.Unlock()
			return nil
		}),
		WithDoModify(func(_ context.Context, _ entroq.Reader, work *Work[string]) (*Result, error) {
			note("work")
			close(done)
			return Modify(work.Task.Delete()), nil
		}),
	)

	runCtx, runCancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- w.Run(runCtx, Watching("q")) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the work phase never ran")
	}
	runCancel()
	<-errc

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"take", "cue", "work"}; !equalStrings(obs.order, want) {
		t.Errorf("phase order = %v, want %v", obs.order, want)
	}
	// The cue names what is held, so the set claimed by TakeDocs is visible.
	if obs.sets != 1 {
		t.Errorf("the cue saw %d doc sets, want the 1 that TakeDocs claimed", obs.sets)
	}
}

// TestCueWorkStallDoesNotPinTheTask is why this phase exists apart from
// DoWork. A cue that never completes blocks its worker -- but nothing is
// renewing the claim, so the lease lapses and another claimant takes the task.
//
// The same wait inside DoWork would renew in the background for as long as the
// worker lived, and no other worker could ever have that task. Move this cue's
// body into WithDoModify and this test goes red.
func TestCueWorkStallDoesNotPinTheTask(t *testing.T) {
	const lease = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f := newCueFixture(ctx, t)

	cued := make(chan struct{})
	var once sync.Once
	w := New[string](f.client,
		WithCueWork(func(ctx context.Context, _ entroq.Reader, _ *Work[string]) error {
			once.Do(func() { close(cued) })
			// Never answered, as a gateway whose client went away. Only the
			// Run ending releases it.
			<-ctx.Done()
			return ctx.Err()
		}),
		WithDoModify(func(_ context.Context, _ entroq.Reader, work *Work[string]) (*Result, error) {
			t.Error("the work phase ran for a task whose cue never completed")
			return Modify(work.Task.Delete()), nil
		}),
	)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	errc := make(chan error, 1)
	go func() { errc <- w.Run(runCtx, Watching("q"), WithLease(lease)) }()

	select {
	case <-cued:
	case <-ctx.Done():
		t.Fatal("the cue never ran")
	}

	// Past the lease the task belongs to whoever asks: the stalled cue holds
	// nothing open.
	var claimed *entroq.Task
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		task, err := f.intruder.TryClaim(ctx, entroq.From("q"))
		if err != nil {
			t.Fatalf("Intruder claim: %v", err)
		}
		if task != nil {
			claimed = task
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if claimed == nil {
		t.Error("no other claimant could take the task: a stalled cue pinned it")
	}

	// Only cancellation ends a stalled cue, and it ends the Run cleanly.
	runCancel()
	if err := <-errc; err != nil {
		t.Errorf("Run: %v", err)
	}
}

// TestCueWorkSentinelActsOnTheTask checks that a sentinel from the cue is taken
// at its word, exactly as one from TakeDocs is, and that the work phase is
// skipped. A sentinel is checked before the deadline so a handler that returns
// one on its way out of a timeout still decides the task's fate.
func TestCueWorkSentinelActsOnTheTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := newCueFixture(ctx, t)

	moved := make(chan struct{})
	var once sync.Once
	w := New[string](f.client,
		WithCueWork(func(context.Context, entroq.Reader, *Work[string]) error {
			once.Do(func() { close(moved) })
			return MoveErrorf("this task can never be cued")
		}),
		WithDoModify(func(_ context.Context, _ entroq.Reader, work *Work[string]) (*Result, error) {
			t.Error("the work phase ran for a task the cue moved")
			return Modify(work.Task.Delete()), nil
		}),
	)

	runCtx, runCancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- w.Run(runCtx, Watching("q")) }()
	select {
	case <-moved:
	case <-ctx.Done():
		t.Fatal("the cue never ran")
	}

	// The move lands in the default error queue for the inbox.
	var tasks []*entroq.Task
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		if tasks, err = f.client.Tasks(ctx, "q/err"); err != nil {
			t.Fatalf("Tasks: %v", err)
		}
		if len(tasks) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(tasks) != 1 {
		t.Errorf("q/err holds %d tasks, want the 1 the cue moved", len(tasks))
	}
	runCancel()
	<-errc
}

// TestThenStopCommitsThenEnds checks that a drain costs nothing: the result
// commits, and only then does the Run end, cleanly.
//
// Cancelling the Run's context would have abandoned the very task the client
// just finished, which is why this rides on the result instead.
func TestThenStopCommitsThenEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := newCueFixture(ctx, t)

	// A second task proves the worker stopped claiming rather than merely
	// finishing what it had.
	if _, err := f.client.Modify(ctx, entroq.InsertingInto("q")); err != nil {
		t.Fatalf("Insert second: %v", err)
	}

	var mu sync.Mutex
	work := 0
	w := New[string](f.client,
		WithDoModify(func(_ context.Context, _ entroq.Reader, work2 *Work[string]) (*Result, error) {
			mu.Lock()
			work++
			mu.Unlock()
			return Modify(work2.Task.Delete()).ThenStop(), nil
		}),
	)

	errc := make(chan error, 1)
	go func() { errc <- w.Run(ctx, Watching("q")) }()

	select {
	case err := <-errc:
		// A clean end, with no cancellation anywhere.
		if err != nil {
			t.Errorf("Run after ThenStop = %v, want nil", err)
		}
	case <-ctx.Done():
		t.Fatal("ThenStop did not end the Run")
	}

	mu.Lock()
	ran := work
	mu.Unlock()
	if ran != 1 {
		t.Errorf("the work phase ran %d times, want 1: the worker kept claiming after ThenStop", ran)
	}

	// The first task committed; the second is untouched and still claimable.
	left, err := f.client.Tasks(ctx, "q")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(left) != 1 {
		t.Errorf("queue holds %d tasks, want the 1 that was never claimed: the committed task did not land", len(left))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
