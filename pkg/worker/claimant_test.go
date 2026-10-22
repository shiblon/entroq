package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// TestConcurrentRunsHoldSetsApart is the guarantee this exists for: two Runs
// sharing one connection must not be able to claim each other's doc sets.
//
// A claimant is a consumer, not a process. Before each Run scoped itself, two
// Runs on one connection were one claimant, so doc-set exclusion -- which keys
// on the claimant -- silently stopped excluding, and each invalidated the
// versions the other held. Nothing reported it.
func TestConcurrentRunsHoldSetsApart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	const (
		queue = "two_runs"
		ns    = "two_runs"
		key   = "contended"
	)
	// One doc set both Runs will want, and two tasks so both get work.
	if _, err := client.Modify(ctx,
		entroq.PuttingDocInto(ns, entroq.WithKeys(key, "")),
		entroq.InsertingInto(queue, entroq.WithValue("a")),
		entroq.InsertingInto(queue, entroq.WithValue("b")),
	); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	// Each task's handler reports who holds the set while it holds it, and
	// stays in the body until released, so the two overlap if they can.
	var mu sync.Mutex
	holders := map[string]int{}
	inBody := make(chan struct{}, 2)
	release := make(chan struct{})

	w := New[string](client,
		WithTakeDocs[string](func(ctx context.Context, _ entroq.Reader, work *Work[string]) (*TakeResult, error) {
			return Take(entroq.ClaimKey(ns, key)), nil
		}),
		WithDoModify[string](func(ctx context.Context, _ entroq.Reader, work *Work[string]) (*Result, error) {
			task, sets := work.Task, work.Sets
			mu.Lock()
			for _, g := range sets {
				holders[g.Claimant]++
			}
			mu.Unlock()
			inBody <- struct{}{}
			<-release
			return Modify(task.Delete()), nil
		}),
	)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	errs := make(chan error, 2)
	for _, name := range []string{"run-1", "run-2"} {
		go func() {
			errs <- w.Run(runCtx, Watching(queue), AsClaimant(name),
				WithLease(10*time.Second), WithBaseRetryDelay(20*time.Millisecond))
		}()
	}

	// One body gets in. The other cannot, because the set it needs is held by
	// a DIFFERENT consumer -- which is the whole point. It retries on
	// contention instead.
	select {
	case <-inBody:
	case err := <-errs:
		t.Fatalf("a Run stopped instead of working: %v", err)
	case <-ctx.Done():
		t.Fatal("neither Run reached its body")
	}
	select {
	case <-inBody:
		t.Fatal("both Runs hold the same doc set at once: they are one consumer, and the exclusion is not working")
	case <-time.After(300 * time.Millisecond):
	}

	close(release)

	// The second body now gets the set, after the first released it.
	select {
	case <-inBody:
	case err := <-errs:
		t.Fatalf("a Run stopped instead of taking the released set: %v", err)
	case <-ctx.Done():
		t.Fatal("the second Run never got the set the first released")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(holders) == 0 {
		t.Fatal("no holder recorded")
	}
	for h := range holders {
		if h != "run-1" && h != "run-2" {
			t.Errorf("set held by %q, want one of the Runs' own claimants", h)
		}
	}
}

// TestWorkersSharingAConnectionAreDistinctConsumers runs two workers on one
// client and checks that their Runs claim as different consumers, neither of
// them the connection itself. A default name derived from the connection and
// a count kept per worker gave both first Runs one claimant, so their doc sets
// excluded neither from the other.
func TestWorkersSharingAConnectionAreDistinctConsumers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener(), entroq.WithClaimantID("conn"))
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	const queue = "default_claimant"
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue), entroq.InsertingInto(queue)); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Each worker holds its task until both have one, so they claim at once.
	seen := make(chan string, 2)
	both := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	claimed := 0
	newWorker := func() *Worker[string] {
		return New[string](client, WithDoModify[string](
			func(ctx context.Context, _ entroq.Reader, work *Work[string]) (*Result, error) {
				task := work.Task
				seen <- task.Claimant
				mu.Lock()
				if claimed++; claimed == 2 {
					once.Do(func() { close(both) })
				}
				mu.Unlock()
				select {
				case <-both:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return Modify(task.Delete()), nil
			}))
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	for range 2 {
		w := newWorker()
		go func() { _ = w.Run(runCtx, Watching(queue), WithLease(10*time.Second)) }()
	}

	var got []string
	for range 2 {
		select {
		case c := <-seen:
			got = append(got, c)
		case <-ctx.Done():
			t.Fatalf("claimed %v before timing out, want two tasks claimed", got)
		}
	}
	if got[0] == got[1] {
		t.Errorf("both workers claimed as %q, want a consumer each", got[0])
	}
	for _, c := range got {
		if c == "conn" {
			t.Errorf("a Run claimed as the connection itself, %q", c)
		}
	}
}
