package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
			WithDoWork(func(ctx context.Context, task *entroq.Task, s string, _ []*entroq.DocSet) error {
				if s != "hi" {
					return errors.New("wrong value")
				}
				return nil
			}),
			WithFinish(func(ctx context.Context, mod Modifier, task *entroq.Task, _ string, _ []*entroq.DocSet) error {
				if _, err := mod.Modify(ctx, task.Delete()); err != nil {
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

func TestWorker_MaxClaimsQuarantinesBeforeHandler(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	const queue = "max_claims"
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue, entroq.WithValue("work"))); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	claimed, err := client.Claim(ctx, entroq.From(queue), entroq.ClaimFor(time.Second))
	if err != nil {
		t.Fatalf("First claim: %v", err)
	}
	// Release it without modifying it, so its claim still counts.
	if _, err := client.UpdateArrival(ctx, entroq.ReadyNow().Tasks(claimed)); err != nil {
		t.Fatalf("Release first claim: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	handlerMade := make(chan struct{}, 1)
	go func() {
		w := New[string](client, WithMakeHandler(func() (Handler[string], error) {
			handlerMade <- struct{}{}
			return nil, errors.New("handler must not be constructed")
		}))
		done <- w.Run(runCtx, Watching(queue), WithMaxClaims(1))
	}()

	waitForTasks(ctx, t, client, queue+"/err", 1)
	runCancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("worker run: %v", err)
	}
	select {
	case <-handlerMade:
		t.Error("handler was constructed for a task over max claims")
	default:
	}

	tasks, err := client.Tasks(ctx, queue+"/err")
	if err != nil {
		t.Fatalf("error queue tasks: %v", err)
	}
	// The move to quarantine is a modification, so it resets the count; the
	// error records the count that sent the task there, and the lease.
	if got := tasks[0].Claims; got != 0 {
		t.Errorf("quarantined task claims = %d, want 0", got)
	}
	if e := tasks[0].Err; !strings.Contains(e, "2 claims without modification") || !strings.Contains(e, "lease ") {
		t.Errorf("quarantined task err = %q, want it to name the claim count and the lease", e)
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
	if err := doWhileRenewing(ctx, client, 6*time.Second, 3*time.Second, held{task: task}, func(ctx context.Context, stop finalizeRenew) error {
		select {
		case <-ctx.Done():
			return fmt.Errorf("doWhileRenewing: %w", ctx.Err())
		case <-time.After(10 * time.Second):
		}
		stable := stop()
		if want, got := task.Version+3, stable.task.Version; want != got {
			t.Errorf("expected version %d after 3 renewals, got %d", want, got)
		}
		return nil
	}); err != nil {
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
		errChan <- doWhileRenewing(ctx, client, 100*time.Millisecond, 50*time.Millisecond, held{task: claimed}, func(ctx context.Context, _ finalizeRenew) error {
			<-ctx.Done()
			return ctx.Err()
		})
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
		entroq.PuttingDoc(&entroq.DocData{Namespace: "ns", Key: "k"}),
	); err != nil {
		t.Fatalf("Insert task+doc: %v", err)
	}

	worked := make(chan error, 1)
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	go func() {
		err := New(client,
			WithTakeDocs(func(_ context.Context, _ *entroq.Task, _ string) (*TakeResult, error) {
				return Take(entroq.ClaimKey("ns", "k")), nil
			}),
			WithDoModify(func(_ context.Context, task *entroq.Task, _ string, sets []*entroq.DocSet) (*Result, error) {
				docs := entroq.DocsIn(sets)
				if len(docs) == 0 {
					return nil, FatalErrorf("expected claimed doc")
				}
				// Sleep past at least one renewal cycle so the doc's version bumps.
				// Finish must fix the version up from docs[0].Version (original) to
				// the renewed version before calling Modify.
				time.Sleep(lease * 3 / 2)
				return Modify(task.Delete(), docs[0].Delete()), nil
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

// TestHeldRenewed checks that a renewal's response moves the task, and each
// set and its docs, to their new versions and claims, and that a response
// missing anything it renewed is an error.
func TestHeldRenewed(t *testing.T) {
	later := time.Now().Add(time.Minute)
	task := &entroq.Task{ID: "t", Version: 3}
	full := &entroq.DocSet{Namespace: "ns", Key: "k", Version: 1, NumDocs: 2, Docs: []*entroq.Doc{
		{Namespace: "ns", ID: "a", Key: "k", Version: 1},
		{Namespace: "ns", ID: "b", Key: "k", Version: 1},
	}}
	empty := &entroq.DocSet{Namespace: "ns", Key: "e", Version: 4}
	h := held{task: task, sets: []*entroq.DocSet{full, empty}}
	resp := &entroq.ModifyResponse{
		ChangedTasks: []*entroq.Task{{ID: "t", Version: 4}},
		ChangedSets: []*entroq.DocSet{
			{Namespace: "ns", Key: "e", Version: 5, Claimant: "me", At: later},
			{Namespace: "ns", Key: "k", Version: 2, Claimant: "me", At: later, NumDocs: 2},
		},
	}
	got, err := h.renewed(resp)
	if err != nil {
		t.Fatalf("Renewed: %v", err)
	}
	if got.task.Version != 4 {
		t.Errorf("Renewed task: got %+v", got.task)
	}
	if g := got.sets[0]; g.Version != 2 || g.Claimant != "me" || !g.At.Equal(later) || len(g.Docs) != 2 {
		t.Errorf("Renewed set: got %+v", g)
	}
	for _, d := range got.sets[0].Docs {
		if d.Version != 2 || d.Claimant != "me" || !d.At.Equal(later) {
			t.Errorf("Renewed member: want it at its set's lock, got %+v", d)
		}
	}
	if e := got.sets[1]; e.Key != "e" || e.Version != 5 || e.Claimant != "me" || len(e.Docs) != 0 {
		t.Errorf("Renewed empty set: got %+v", e)
	}
	if full.Version != 1 || full.Docs[0].Version != 1 {
		t.Error("renewed changed the sets it was given")
	}

	missingSet := &entroq.ModifyResponse{ChangedTasks: resp.ChangedTasks, ChangedSets: resp.ChangedSets[:1]}
	if _, err := h.renewed(missingSet); err == nil {
		t.Error("Response missing a set: want an error")
	}
	if _, err := h.renewed(&entroq.ModifyResponse{ChangedSets: resp.ChangedSets}); err == nil {
		t.Error("Response missing the task: want an error")
	}
}

func TestErrQTemplate(t *testing.T) {
	for template, want := range map[string]string{
		"":                  "jobs/err",
		"{inbox}/err":       "jobs/err",
		"{inbox}/dead":      "jobs/dead",
		"quarantine":        "quarantine",
		"x/{inbox}/{inbox}": "x/jobs/jobs",
	} {
		if got := ErrQTemplate(template)("jobs"); got != want {
			t.Errorf("ErrQTemplate(%q)(jobs) = %q, want %q", template, got, want)
		}
	}
}

// TestSlowTakeDocsRenewsAtOnce checks that when claiming the sets takes more
// than half the lease, the first renewal comes at once rather than half a
// lease later, when the task's claim would already have lapsed.
func TestSlowTakeDocsRenewsAtOnce(t *testing.T) {
	const lease = 400 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	backend, err := eqmem.Opener()(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	shared := func(context.Context) (entroq.Backend, error) { return backend, nil }
	client, err := entroq.New(ctx, shared)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()
	// The intruder shares the store, not the claimant; closing the client
	// above closes the store for both.
	intruder, err := entroq.New(ctx, shared, entroq.WithClaimantID("intruder"))
	if err != nil {
		t.Fatalf("New intruder: %v", err)
	}
	if _, err := client.Modify(ctx, entroq.InsertingInto("q")); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	working := make(chan time.Time, 1)
	finish := make(chan bool)
	w := New[string](client,
		WithTakeDocs(func(context.Context, *entroq.Task, string) (*TakeResult, error) {
			time.Sleep(lease * 7 / 10)
			return Take(entroq.ClaimKey("ns", "k")), nil
		}),
		WithDoModify(func(_ context.Context, task *entroq.Task, _ string, _ []*entroq.DocSet) (*Result, error) {
			working <- time.Now()
			<-finish
			return Modify(task.Delete()), nil
		}),
	)
	runCtx, runCancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- w.Run(runCtx, Watching("q"), WithLease(lease)) }()

	started := <-working
	// The task was claimed about 0.7 leases before work started; its first
	// lease ends 0.3 leases from now. Try to take it just after that.
	time.Sleep(time.Until(started.Add(lease * 4 / 10)))
	task, err := intruder.TryClaim(ctx, entroq.From("q"))
	if err != nil {
		t.Fatalf("Intruder claim: %v", err)
	}
	if task != nil {
		t.Error("Another claimant took the task: its claim lapsed before the first renewal")
	}
	close(finish)
	runCancel()
	if err := <-errCh; err != nil {
		t.Errorf("Run: %v", err)
	}
}

// TestDocsExpireWithTheirTask checks that the worker claims a task's doc sets
// as itself, until the task's own arrival time, so that if the worker dies they
// come free together, before any renewal, whatever lease or claimant the
// handler named.
func TestDocsExpireWithTheirTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()
	if _, err := client.Modify(ctx, entroq.InsertingInto("q")); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	type seen struct {
		task, a, b time.Time
		holders    []string
	}
	got := make(chan seen, 1)
	w := New[string](client,
		WithTakeDocs(func(context.Context, *entroq.Task, string) (*TakeResult, error) {
			// The lease and claimant are the worker's: these are ignored.
			return Take(entroq.ClaimKey("ns", "a"), entroq.ClaimKey("ns", "b").WithoutMembers(),
				entroq.ClaimingSetsFor(time.Hour), entroq.ClaimingSetsAs("someone else")), nil
		}),
		WithDoModify(func(_ context.Context, task *entroq.Task, _ string, sets []*entroq.DocSet) (*Result, error) {
			got <- seen{task.At, sets[0].At, sets[1].At, []string{sets[0].Claimant, sets[1].Claimant}}
			return Modify(task.Delete()), nil
		}),
	)
	runCtx, runCancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- w.Run(runCtx, Watching("q"), WithLease(time.Minute)) }()

	s := <-got
	if !s.a.Equal(s.task) || !s.b.Equal(s.task) {
		t.Errorf("Sets held until %v and %v, want both until the task's arrival %v", s.a, s.b, s.task)
	}
	for _, h := range s.holders {
		if h != client.ClientID {
			t.Errorf("Set held by %q, want the worker's own claimant %q", h, client.ClientID)
		}
	}
	runCancel()
	if err := <-errCh; err != nil {
		t.Errorf("Run: %v", err)
	}
}
