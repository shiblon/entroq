package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

func newShutdownEQ(ctx context.Context, t *testing.T) *entroq.EntroQ {
	t.Helper()
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// runAsync starts w.Run on queue and returns a channel carrying its result.
func runAsync(ctx context.Context, w *Worker[string], queue string) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- w.Run(ctx, Watching(queue), WithLease(time.Second)) }()
	return errc
}

func wantRunResult(ctx context.Context, t *testing.T, errc <-chan error, want error) {
	t.Helper()
	select {
	case err := <-errc:
		if !errors.Is(err, want) {
			t.Errorf("Run: got %v, want %v", err, want)
		}
	case <-ctx.Done():
		t.Fatal("Run did not return")
	}
}

// TestShutdown_Idle: a Run blocked in claim on an empty queue stops at once.
func TestShutdown_Idle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := newShutdownEQ(ctx, t)

	w := New(client, WithDoModify(func(_ context.Context, task *entroq.Task, _ string, _ []*entroq.DocSet) (*Result, error) {
		return Modify(task.Delete()), nil
	}))
	var errcs []<-chan error
	for range 3 {
		errcs = append(errcs, runAsync(ctx, w, "q"))
	}
	time.Sleep(50 * time.Millisecond) // let the Runs block in claim

	if err := w.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for _, errc := range errcs {
		wantRunResult(ctx, t, errc, nil)
	}
}

// TestShutdown_DrainsInFlight: the task in hand is finished and committed, and
// no further task is claimed.
func TestShutdown_DrainsInFlight(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := newShutdownEQ(ctx, t)

	if _, err := client.Modify(ctx, entroq.InsertingInto("q", entroq.WithValue("first"))); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	w := New(client, WithDoModify(func(_ context.Context, task *entroq.Task, _ string, _ []*entroq.DocSet) (*Result, error) {
		close(started)
		<-release
		return Modify(task.Delete()), nil
	}))
	errc := runAsync(ctx, w, "q")
	<-started

	// Queued behind the one in hand: the drain must leave it alone.
	if _, err := client.Modify(ctx, entroq.InsertingInto("q", entroq.WithValue("second"))); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	shutErr := make(chan error, 1)
	go func() { shutErr <- w.Shutdown(ctx) }()
	select {
	case err := <-shutErr:
		t.Fatalf("Shutdown returned before the task finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)

	if err := <-shutErr; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	wantRunResult(ctx, t, errc, nil)
	tasks, err := client.Tasks(ctx, "q")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Claims != 0 {
		t.Fatalf("got %d tasks (%v), want only the unclaimed second", len(tasks), tasks)
	}
}

// TestShutdown_DeadlineCancelsHandlers: when the drain runs out of time, the
// handler's context is canceled and Shutdown reports the deadline.
func TestShutdown_DeadlineCancelsHandlers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := newShutdownEQ(ctx, t)

	if _, err := client.Modify(ctx, entroq.InsertingInto("q", entroq.WithValue("stuck"))); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	started := make(chan struct{})
	w := New(client, WithDoModify(func(ctx context.Context, _ *entroq.Task, _ string, _ []*entroq.DocSet) (*Result, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	errc := runAsync(ctx, w, "q")
	<-started

	sctx, scancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer scancel()
	if err := w.Shutdown(sctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown: got %v, want deadline exceeded", err)
	}
	wantRunResult(ctx, t, errc, nil)
}

// TestShutdown_RunAfter: a Run on a shut-down worker refuses to start, even
// while another Run is still draining.
func TestShutdown_RunAfter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := newShutdownEQ(ctx, t)

	w := New(client, WithDoModify(func(_ context.Context, task *entroq.Task, _ string, _ []*entroq.DocSet) (*Result, error) {
		return Modify(task.Delete()), nil
	}))
	if err := w.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := w.Run(ctx, Watching("q")); !errors.Is(err, ErrShutdown) {
		t.Fatalf("Run after Shutdown: got %v, want ErrShutdown", err)
	}
}

// TestShutdown_ConcurrentRuns races Run starts against Shutdown: every Run
// either refuses or is waited for. Meant for -race.
func TestShutdown_ConcurrentRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := newShutdownEQ(ctx, t)

	w := New(client, WithDoModify(func(_ context.Context, task *entroq.Task, _ string, _ []*entroq.DocSet) (*Result, error) {
		return Modify(task.Delete()), nil
	}))
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.Run(ctx, Watching("q")); err != nil && !errors.Is(err, ErrShutdown) {
				t.Errorf("Run: %v", err)
			}
		}()
	}
	if err := w.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	wg.Wait()
}
