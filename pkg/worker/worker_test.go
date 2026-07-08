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

func TestWorker_Basic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	if _, err := client.Modify(ctx, entroq.InsertingInto("test_q", entroq.WithValue("hi"))); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	done := make(chan bool, 1)
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		w := New(client,
			WithDoWork(func(ctx context.Context, task *entroq.Task, s string, _ []*entroq.Doc) error {
				if s != "hi" {
					return errors.New("wrong value")
				}
				return nil
			}),
			WithFinish(func(ctx context.Context, task *entroq.Task, _ string, _ []*entroq.Doc) error {
				if _, err := client.Modify(ctx, task.Delete()); err != nil {
					return err
				}
				done <- true
				return nil
			}),
		)
		if err := w.Run(runCtx, Watching("test_q")); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Worker run: %v", err)
		}
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for worker")
	}
}

// TestWorkerRenewal verifies that doWhileRenewing actually renews the claim at
// the expected interval and that stop() returns stable (finalized) versions.
func TestWorkerRenewal(t *testing.T) {
	// 10 s work + generous headroom for renewal timing.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	const queue = "worker_renewal"
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue)); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	task, err := client.Claim(ctx, entroq.From(queue), entroq.ClaimFor(6*time.Second))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Renewal fires at interval/2 = 3 s; 10 s → 3 renewals expected.
	if err := doWhileRenewing(ctx, client, func(ctx context.Context, stop finalizeRenew) error {
		select {
		case <-ctx.Done():
			return fmt.Errorf("doWhileRenewing: %w", ctx.Err())
		case <-time.After(10 * time.Second):
		}
		stable := stop()
		if want, got := task.Version+3, stable.Tasks[0].Version; want != got {
			t.Errorf("expected version %d after 3 renewals, got %d", want, got)
		}
		return nil
	}, entroq.RenewingTask(task), entroq.WithRenewInterval(6*time.Second)); err != nil {
		t.Fatalf("doWhileRenewing: %v", err)
	}
}

// TestDoWhileRenewing_ImmediateCancellationOnLeaseLoss verifies that
// doWhileRenewing cancels the work context promptly when renewal fails with a
// DependencyError (i.e. the task was stolen or deleted under the worker).
func TestDoWhileRenewing_ImmediateCancellationOnLeaseLoss(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	const queue = "cancel_on_loss"
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithValue("work"))); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	claimed, err := client.Claim(ctx, entroq.From(queue), entroq.ClaimFor(10*time.Second))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	errChan := make(chan error, 1)
	go func() {
		errChan <- doWhileRenewing(ctx, client, func(ctx context.Context, _ finalizeRenew) error {
			<-ctx.Done()
			return ctx.Err()
		}, entroq.RenewingTask(claimed), entroq.WithRenewInterval(100*time.Millisecond))
	}()

	// Delete the task to break renewal.
	if _, err := client.Modify(ctx, claimed.Delete()); err != nil {
		t.Fatalf("Delete claimed task: %v", err)
	}

	select {
	case err := <-errChan:
		if _, ok := entroq.AsDependency(err); !ok {
			t.Errorf("expected DependencyError, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("timed out: cancellation after lease loss took too long")
	}
}

// TestDoModify_DocVersionFixedAfterRenewal verifies that doModifyHandler.Finish
// patches doc versions to their renewed state before calling Modify. Without the
// fix, returning a delete for a doc using the original (pre-renewal) version would
// produce a DependencyError and silently leave the doc in place.
func TestDoModify_DocVersionFixedAfterRenewal(t *testing.T) {
	const lease = 200 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()

	if _, err := client.Modify(ctx,
		entroq.InsertingInto("q", entroq.WithValue("work")),
		entroq.InsertingDoc(&entroq.DocData{Namespace: "ns", Key: "k"}),
	); err != nil {
		t.Fatalf("Insert task+doc: %v", err)
	}

	worked := make(chan error, 1)
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		err := New(client,
			WithTakeDocs(func(_ context.Context, _ *entroq.Task, _ string) ([]*entroq.DocClaim, error) {
				return []*entroq.DocClaim{entroq.ClaimKey("ns", "k")}, nil
			}),
			WithDoModify(func(_ context.Context, task *entroq.Task, _ string, docs []*entroq.Doc) ([]entroq.ModifyArg, error) {
				if len(docs) == 0 {
					return nil, FatalErrorf("expected claimed doc")
				}
				// Sleep past at least one renewal cycle so the doc's version bumps.
				// Finish must fix the version up from docs[0].Version (original) to
				// the renewed version before calling Modify.
				time.Sleep(lease * 3 / 2)
				return []entroq.ModifyArg{task.Delete(), docs[0].Delete()}, nil
			}),
		).Run(runCtx, Watching("q"), WithLease(lease))
		worked <- err
	}()

	// Wait long enough for the task to be processed, then stop the worker.
	time.Sleep(lease * 5)
	runCancel()

	if err := <-worked; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Worker: %v", err)
	}

	// The doc must be gone — a stale version would cause a silent DependencyError
	// in Finish and leave the doc in place.
	remaining, err := client.Docs(ctx, &entroq.DocQuery{Namespace: "ns"})
	if err != nil {
		t.Fatalf("Docs: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("doc not deleted: version fix-up likely missing (found %d docs)", len(remaining))
	}
}
