package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// TestWorkTimeoutRetriesTheTask covers what a body that runs too long costs:
// the task, once, and not the worker. A hang that was bad luck comes back and
// succeeds.
func TestWorkTimeoutRetriesTheTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()

	const queue = "work_timeout"
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithValue("slow"))); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	started := make(chan struct{}, 4)
	stopped := make(chan error, 4)
	w := New[string](client, WithDoWork(func(ctx context.Context, _ entroq.Reader, tRun *TaskRun[string]) error {
		started <- struct{}{}
		// A cooperative body: it is asked to stop, and it does.
		<-ctx.Done()
		stopped <- ctx.Err()
		return ctx.Err()
	}))

	runCtx, runCancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- w.Run(runCtx, Watching(queue),
			WithLease(10*time.Second),
			WithWorkTimeout(300*time.Millisecond),
			WithBaseRetryDelay(100*time.Millisecond),
		)
	}()

	// The body is entered, asked to stop, and the task comes back for another
	// go -- which is the whole contract.
	for i := range 2 {
		select {
		case <-started:
		case err := <-errCh:
			t.Fatalf("Run stopped instead of retrying the task (attempt %d): %v", i+1, err)
		case <-ctx.Done():
			t.Fatalf("Body was not entered for attempt %d", i+1)
		}
		select {
		case err := <-stopped:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("Body's context ended with %v, want a deadline", err)
			}
		case <-ctx.Done():
			t.Fatalf("Body was never asked to stop on attempt %d", i+1)
		}
	}

	runCancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Run: %v", err)
	}

	// Retried, not quarantined: the task is back in its own queue with the
	// reason recorded, and its attempts counted.
	tasks, err := client.Tasks(ctx, queue)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("Queue %q holds %d tasks, want the retried one", queue, len(tasks))
	}
	if tasks[0].Attempt < 2 {
		t.Errorf("Attempt count %d, want at least 2: a timeout must count against WithMaxAttempts", tasks[0].Attempt)
	}
	if !strings.Contains(tasks[0].Err, "did not finish within") {
		t.Errorf("Recorded error %q, want it to name the timeout", tasks[0].Err)
	}
	if moved, _ := client.Tasks(ctx, queue+"/err"); len(moved) != 0 {
		t.Errorf("Task quarantined on a timeout: %v", moved)
	}
}

// TestWorkTimeoutExhaustsAttemptsThenQuarantines covers the other half: a task
// that hangs every time is for a person to look at, not for a worker to keep
// picking up.
func TestWorkTimeoutExhaustsAttemptsThenQuarantines(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()

	const queue = "work_timeout_attempts"
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithValue("always slow"))); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	w := New[string](client, WithDoWork(func(ctx context.Context, _ entroq.Reader, tRun *TaskRun[string]) error {
		<-ctx.Done()
		return ctx.Err()
	}))

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- w.Run(runCtx, Watching(queue),
			WithLease(10*time.Second),
			WithWorkTimeout(150*time.Millisecond),
			WithBaseRetryDelay(50*time.Millisecond),
			WithMaxAttempts(2),
		)
	}()

	deadline := time.After(20 * time.Second)
	for {
		moved, err := client.Tasks(ctx, queue+"/err")
		if err != nil {
			t.Fatalf("Tasks: %v", err)
		}
		if len(moved) == 1 {
			if !strings.Contains(moved[0].Err, "did not finish within") {
				t.Errorf("Quarantined with %q, want it to name the timeout", moved[0].Err)
			}
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("Run stopped before the task was quarantined: %v", err)
		case <-deadline:
			t.Fatal("Task was never quarantined after exhausting its attempts")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestWorkTimeoutZeroMeansNever is the default, and the reason it is: a body
// that idles for a long time on purpose must not be interrupted.
func TestWorkTimeoutZeroMeansNever(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()

	const queue = "work_timeout_off"
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue)); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	done := make(chan struct{})
	w := New[string](client, WithDoWork(func(ctx context.Context, _ entroq.Reader, tRun *TaskRun[string]) error {
		// Longer than any timeout this test could have set by accident, and
		// longer than the lease, so renewal carries it.
		select {
		case <-time.After(1200 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		close(done)
		return nil
	}))

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- w.Run(runCtx, Watching(queue), WithLease(time.Second))
	}()

	select {
	case <-done:
	case err := <-errCh:
		t.Fatalf("Run stopped before the body finished: %v", err)
	case <-ctx.Done():
		t.Fatal("Body did not run to completion with no work timeout set")
	}
}
