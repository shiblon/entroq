package entroq_test

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/backend/eqmem"
	"github.com/shiblon/entroq/worker"
)

func TestDoWithRenewAll_ImmediateCancellationOnLeaseLoss(t *testing.T) {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("failed to create eq: %v", err)
	}
	defer eq.Close()

	queue := "/test/cancel"
	_, _, err = eq.Modify(ctx, entroq.InsertingInto(queue, entroq.WithValue("work")))
	if err != nil {
		t.Fatalf("failed to insert: %v", err)
	}
	// 1. Claim the task.
	claimed, err := eq.Claim(ctx, entroq.From(queue), entroq.ClaimFor(10*time.Second))
	if err != nil {
		t.Fatalf("failed to claim: %v", err)
	}

	// 2. Start renewing.
	errChan := make(chan error, 1)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	go func() {
		// Use a short renewal interval to speed up the test.
		errChan <- worker.DoWithRenewAll(ctx, eq, []*entroq.Task{claimed}, 100*time.Millisecond, func(ctx context.Context, stop worker.FinalizeRenewAll) error {
			// Wait for the context to be canceled from the outside.
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	// 3. Poach the task from underneath the worker.
	// Bump version by claiming or deleting. Same claimant, so no need to spoof.
	if _, _, err := eq.Modify(ctx, claimed.Delete()); err != nil {
		t.Fatalf("failed to steal task: %v", err)
	}

	// 4. Verify that DoWithRenewAll returns DependencyError (wrapped or direct)
	// and that it happened quickly.
	select {
	case err := <-errChan:
		if _, ok := entroq.AsDependency(err); !ok {
			t.Errorf("expected DependencyError, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Errorf("timed out waiting for worker cancellation after lease loss")
	}
}
