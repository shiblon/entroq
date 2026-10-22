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

// TestCancellationWinsOverSentinels: a body that sees its worker canceled and
// returns a retry or a move, as one whose read failed with the cancellation
// would, leaves the task as it was: still claimed, its attempt uncounted, in
// its own queue. The lease decides what happens next, on every backend alike.
func TestCancellationWinsOverSentinels(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sentinel func(error) error
	}{
		{"retry", func(err error) error { return RetryErrorf("read failed: %v", err) }},
		{"move", func(err error) error { return MoveErrorf("read failed: %v", err) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client, err := entroq.New(ctx, eqmem.Opener())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer client.Close()

			const queue = "cancel_wins"
			if _, err := client.Modify(ctx, entroq.InsertingInto(queue)); err != nil {
				t.Fatalf("Insert: %v", err)
			}
			before, err := client.Tasks(ctx, queue)
			if err != nil || len(before) != 1 {
				t.Fatalf("Tasks before: %v, %v", before, err)
			}

			// A sentinel applied anyway would hand the task straight back, so
			// the body can be entered more than once.
			entered := make(chan struct{})
			var once sync.Once
			w := New[string](client, WithDoWork(func(ctx context.Context, _ entroq.Reader, work *Work[string]) error {
				once.Do(func() { close(entered) })
				<-ctx.Done()
				return tc.sentinel(ctx.Err())
			}))

			runCtx, runCancel := context.WithCancel(ctx)
			errCh := make(chan error, 1)
			go func() {
				errCh <- w.Run(runCtx, Watching(queue), WithLease(10*time.Second), WithBaseRetryDelay(0))
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("body never entered")
			}
			runCancel()
			// A sentinel applied anyway can keep a canceled Run going: the task
			// comes straight back, and eqmem grants a claim on an ended context.
			select {
			case err := <-errCh:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("Run: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not stop after its context was canceled")
			}

			after, err := client.Tasks(ctx, queue)
			if err != nil {
				t.Fatalf("Tasks after: %v", err)
			}
			if len(after) != 1 {
				t.Fatalf("queue %q holds %d tasks, want the one task left in place", queue, len(after))
			}
			got := after[0]
			if got.Attempt != before[0].Attempt || got.Err != "" {
				t.Errorf("task recorded attempt %d, err %q: the %s was applied", got.Attempt, got.Err, tc.name)
			}
			if !got.At.After(time.Now()) {
				t.Errorf("task is available at %v, want it still claimed until its lease ends", got.At)
			}
			if moved, _ := client.Tasks(ctx, queue+"/err"); len(moved) != 0 {
				t.Errorf("task moved to the error queue: %v", moved)
			}
		})
	}
}
