package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// TestOnSuccess_RunsOnSuccess checks that a Result's OnSuccess runs once the
// task is handled successfully, and only after the modifications commit (the
// task is already gone by the time it fires).
func TestOnSuccess_RunsOnSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	const q = "onsuccess_q"
	if _, err := client.Modify(ctx, entroq.InsertingInto(q, entroq.WithValue("x"))); err != nil {
		t.Fatalf("insert: %v", err)
	}

	ran := make(chan struct{}, 1)
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	done := make(chan error, 1)
	go func() {
		w := New(client,
			WithDoModify(func(ctx context.Context, task *entroq.Task, _ string, _ []*entroq.Doc) (*Result, error) {
				return Modify(task.Delete()).OnSuccess(func(context.Context) error {
					ran <- struct{}{}
					return nil
				}), nil
			}),
		)
		done <- w.Run(runCtx, Watching(q))
	}()

	select {
	case <-ran:
	case <-ctx.Done():
		t.Fatal("OnSuccess did not run after a successful handle")
	}
	// OnSuccess runs only after the commit, so the task is already gone.
	if err := client.WaitQueuesEmpty(ctx, entroq.MatchExact(q)); err != nil {
		t.Fatalf("queue not empty after commit: %v", err)
	}
	runCancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("worker run: %v", err)
	}
}

// TestOnSuccess_FatalStopsWorker locks in the escalation path: OnSuccess is
// best-effort by default, but returning a FatalError from it stops the worker.
// The task still commits first (OnSuccess runs post-commit), so the queue is
// empty by the time Run returns the fatal error.
func TestOnSuccess_FatalStopsWorker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	const q = "onsuccess_fatal_q"
	if _, err := client.Modify(ctx, entroq.InsertingInto(q, entroq.WithValue("x"))); err != nil {
		t.Fatalf("insert: %v", err)
	}

	w := New(client,
		WithDoModify(func(ctx context.Context, task *entroq.Task, _ string, _ []*entroq.Doc) (*Result, error) {
			return Modify(task.Delete()).OnSuccess(func(context.Context) error {
				return FatalErrorf("stop the worker")
			}), nil
		}),
	)
	// A FatalError from OnSuccess bubbles out of Run rather than being logged.
	err = w.Run(ctx, Watching(q))
	if err == nil {
		t.Fatal("expected worker to exit with the FatalError from OnSuccess")
	}
	if _, ok := AsFatal(err); !ok {
		t.Fatalf("expected a FatalError to propagate, got %v", err)
	}
	// The task committed before the worker stopped: OnSuccess runs post-commit.
	if err := client.WaitQueuesEmpty(ctx, entroq.MatchExact(q)); err != nil {
		t.Fatalf("queue not empty: task should have committed before the fatal exit: %v", err)
	}
}

// TestOnSuccess_SkippedOnError locks in the gate that matters: when the handler
// returns an error, its OnSuccess must not run -- the transaction is canceled,
// there is no success to build on. The handler attaches a hook AND returns a
// RetryError; the worker discards the Result and quarantines the task, and the
// hook never fires.
func TestOnSuccess_SkippedOnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	const inbox, errq = "onsuccess_err_in", "onsuccess_err/dead"
	if _, err := client.Modify(ctx, entroq.InsertingInto(inbox, entroq.WithValue("x"))); err != nil {
		t.Fatalf("insert: %v", err)
	}

	ran := make(chan struct{}, 1)
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	done := make(chan error, 1)
	go func() {
		w := New(client,
			WithDoModify(func(ctx context.Context, task *entroq.Task, _ string, _ []*entroq.Doc) (*Result, error) {
				// A hook is attached, but the handler errors: OnSuccess must not run.
				return Modify(task.Delete()).OnSuccess(func(context.Context) error {
					ran <- struct{}{}
					return nil
				}), RetryErrorf("boom").OrMoveTo(errq)
			}),
		)
		// maxAttempts=1: the first failure exhausts and quarantines to errq.
		done <- w.Run(runCtx, Watching(inbox), WithMaxAttempts(1))
	}()

	// Wait for the task to land in the error queue.
	deadline := time.After(5 * time.Second)
	for {
		tasks, err := client.Tasks(ctx, errq)
		if err != nil {
			t.Fatalf("tasks: %v", err)
		}
		if len(tasks) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("task was never quarantined")
		case <-time.After(20 * time.Millisecond):
		}
	}
	// The hook must not have fired.
	select {
	case <-ran:
		t.Fatal("OnSuccess ran despite the handler returning an error")
	default:
	}
	runCancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("worker run: %v", err)
	}
}
