package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// countingHandler reports, per task, how many times this handler INSTANCE has
// run DoWork. With a fresh handler constructed per task (the contract), every
// task observes a count of 1. If the worker reused one handler instance across
// tasks (the pre-fix once-per-Run behavior), the count would climb 1, 2, 3, ...
// which is exactly the per-task state leak that produced the handoffworker
// stale-tombstone bug.
type countingHandler struct {
	eqc      *entroq.EntroQ
	observed chan<- int
	uses     int
}

func (h *countingHandler) TakeDocs(context.Context, *entroq.Task, string) ([]*entroq.DocClaim, error) {
	return nil, nil
}

func (h *countingHandler) DoWork(ctx context.Context, task *entroq.Task, _ string, _ []*entroq.Doc) error {
	h.uses++
	h.observed <- h.uses
	return nil
}

func (h *countingHandler) Finish(ctx context.Context, task *entroq.Task, _ string, _ []*entroq.Doc) error {
	_, err := h.eqc.Modify(ctx, task.Delete())
	return err
}

// TestWorker_FreshHandlerPerTask locks in the contract that makeHandler runs
// once per task, so per-task handler state is isolated by construction and can
// never leak across the Run loop. This is the regression guard for the
// handoffworker stale-state bug: the worker package, not each caller, owns per-task
// freshness. Revert makeHandler to once-per-Run and this test goes red (the
// observed counts become 1, 2, 3 instead of all 1).
func TestWorker_FreshHandlerPerTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	const nTasks = 3
	for i := 0; i < nTasks; i++ {
		if _, err := client.Modify(ctx, entroq.InsertingInto("test_q", entroq.WithValue("t"))); err != nil {
			t.Fatalf("insert task %d: %v", i, err)
		}
	}

	// Buffered so the worker never blocks sending, even if the reused-handler
	// regression makes it send nTasks values from a single instance.
	observed := make(chan int, nTasks)
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		w := New(client, WithMakeHandler(func() (Handler[string], error) {
			return &countingHandler{eqc: client, observed: observed}, nil
		}))
		if err := w.Run(runCtx, Watching("test_q")); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("worker run: %v", err)
		}
	}()

	for i := 0; i < nTasks; i++ {
		select {
		case uses := <-observed:
			if uses != 1 {
				t.Errorf("task %d observed handler use-count %d, want 1: the handler instance was reused across tasks, so per-task state leaked", i, uses)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for task %d of %d", i, nTasks)
		}
	}
	runCancel()
}
