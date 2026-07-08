package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// TestSentinelErrors_API covers the fluent constructors, setters, and As
// predicates, including detection through an fmt.Errorf %w wrap.
func TestSentinelErrors_API(t *testing.T) {
	re := RetryErrorf("boom %d", 3).After(5 * time.Second).OrMoveTo("/q/err")
	if got, want := re.Error(), "boom 3"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !re.hasAfter || re.after != 5*time.Second {
		t.Errorf("After not recorded: hasAfter=%v after=%v", re.hasAfter, re.after)
	}
	if re.moveTo != "/q/err" {
		t.Errorf("OrMoveTo not recorded: %q", re.moveTo)
	}

	// Detected through a wrap, and not confused with the other kinds.
	wrapped := fmt.Errorf("context: %w", re)
	if got, ok := AsRetry(wrapped); !ok || got != re {
		t.Errorf("AsRetry(wrapped) = %v, %v; want the original RetryError", got, ok)
	}
	if _, ok := AsMove(wrapped); ok {
		t.Error("AsMove matched a RetryError")
	}
	if _, ok := AsFatal(wrapped); ok {
		t.Error("AsFatal matched a RetryError")
	}

	if got, ok := AsMove(MoveErrorf("nope").To("/q/dead")); !ok || got.to != "/q/dead" {
		t.Errorf("AsMove(MoveError.To) = %v, %v; want to=/q/dead", got, ok)
	}
	if _, ok := AsFatal(FatalErrorf("stop")); !ok {
		t.Error("AsFatal(FatalError) = false")
	}
}

// TestRetryError_OrMoveTo verifies the dispatch honors OrMoveTo: when a retry
// exhausts its attempts, the task lands in the error's queue, not the worker's
// default ErrQMap queue.
func TestRetryError_OrMoveTo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	const inbox, customErr = "in", "custom/dead"
	if _, err := client.Modify(ctx, entroq.InsertingInto(inbox, entroq.WithValue("x"))); err != nil {
		t.Fatalf("insert: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	done := make(chan error, 1)
	go func() {
		w := New(client, WithDoWork(func(context.Context, *entroq.Task, string, []*entroq.Doc) error {
			return RetryErrorf("always fails").OrMoveTo(customErr)
		}))
		// maxAttempts=1: the first failure (attempt 0) exhausts and quarantines.
		done <- w.Run(runCtx, Watching(inbox), WithMaxAttempts(1))
	}()

	waitForTasks(ctx, t, client, customErr, 1)
	runCancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("worker run: %v", err)
	}
}

// TestRetryError_After verifies the dispatch honors After: the retried task's
// arrival is pushed by the error's delay, overriding the worker's base delay.
func TestRetryError_After(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	const q = "retry_after"
	if _, err := client.Modify(ctx, entroq.InsertingInto(q, entroq.WithValue("x"))); err != nil {
		t.Fatalf("insert: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	done := make(chan error, 1)
	go func() {
		w := New(client, WithDoWork(func(context.Context, *entroq.Task, string, []*entroq.Doc) error {
			return RetryErrorf("transient").After(time.Hour)
		}))
		// maxAttempts 0 => never quarantines; base delay left at its 30s default,
		// which After(1h) must override.
		done <- w.Run(runCtx, Watching(q))
	}()

	// The handler always fails, so after one retry the task sits in q with a
	// far-future arrival and the worker blocks claiming it. Wait for that retry.
	deadline := time.After(5 * time.Second)
	var retried *entroq.Task
	for retried == nil {
		tasks, err := client.Tasks(ctx, q)
		if err != nil {
			t.Fatalf("tasks: %v", err)
		}
		if len(tasks) == 1 && tasks[0].Attempt >= 1 {
			retried = tasks[0]
			break
		}
		select {
		case <-deadline:
			t.Fatalf("task was not retried in time (tasks=%d)", len(tasks))
		case <-time.After(20 * time.Millisecond):
		}
	}
	runCancel()
	<-done

	if min := time.Now().Add(59 * time.Minute); !retried.At.After(min) {
		t.Errorf("retried task At = %v, want after ~1h (%v); After(1h) was not applied", retried.At, min)
	}
}

// waitForTasks polls until queue holds exactly n tasks or the context/timeout expires.
func waitForTasks(ctx context.Context, t *testing.T, client *entroq.EntroQ, queue string, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		tasks, err := client.Tasks(ctx, queue)
		if err != nil {
			t.Fatalf("tasks(%q): %v", queue, err)
		}
		if len(tasks) == n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("queue %q never reached %d tasks (last=%d)", queue, n, len(tasks))
		case <-time.After(20 * time.Millisecond):
		}
	}
}
