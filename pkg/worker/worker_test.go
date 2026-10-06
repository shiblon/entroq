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
			WithDoWork(func(ctx context.Context, _ entroq.Reader, tRun *TaskRun[string]) error {
				s := tRun.Value
				if s != "hi" {
					return errors.New("wrong value")
				}
				return nil
			}),
			WithFinish(func(ctx context.Context, eqc entroq.Client, tRun *TaskRun[string]) error {
				mod := eqc
				task := tRun.Task
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
	if _, err := client.Modify(ctx, claimed.Change(entroq.ArrivalTimeBy(0))); err != nil {
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

	// The cadence is derived from the task, not given, so derive the expected
	// renewal count the same way rather than hard-coding one: a count tied to
	// the fraction in renewInterval would fail as a mystery if that changed,
	// while this keeps asserting what the test is about, that renewal actually
	// fires on the cadence and that stop() returns the versions it left behind.
	const working = 10 * time.Second
	wantRenewals := int32(working / renewInterval(task))
	if wantRenewals < 2 {
		t.Fatalf("test needs at least two renewals in %v, got %d (interval %v)", working, wantRenewals, renewInterval(task))
	}
	if renewErr, err := doWhileRenewing(ctx, client, 0, held{task: task}, nil, func(ctx context.Context, stop finalizeRenew) error {
		select {
		case <-ctx.Done():
			return fmt.Errorf("doWhileRenewing: %w", ctx.Err())
		case <-time.After(working):
		}
		stable := stop()
		if want, got := task.Version+wantRenewals, stable.task.Version; want != got {
			t.Errorf("expected version %d after %d renewals, got %d", want, wantRenewals, got)
		}
		return nil
	}); renewErr != nil || err != nil {
		t.Fatalf("doWhileRenewing: %v, %v", renewErr, err)
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
	// The cadence is two thirds of the granted lease, so claim for a short one:
	// renewal has to fire, and find the task gone, well inside this test's
	// budget. The assertion is about what a failed renewal does, not about how
	// long the lease was.
	claimed, err := client.Claim(ctx, entroq.From(queue), entroq.ClaimFor(150*time.Millisecond))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	errChan := make(chan error, 1)
	go func() {
		renewErr, _ := doWhileRenewing(ctx, client, 0, held{task: claimed}, nil, func(ctx context.Context, _ finalizeRenew) error {
			<-ctx.Done()
			return ctx.Err()
		})
		errChan <- renewErr
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
			WithTakeDocs(func(_ context.Context, _ entroq.Reader, tRun *TaskRun[string]) (*TakeResult, error) {
				return Take(entroq.ClaimKey("ns", "k")), nil
			}),
			WithDoModify(func(_ context.Context, _ entroq.Reader, tRun *TaskRun[string]) (*Result, error) {
				task, sets := tRun.Task, tRun.Sets
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

	// A reply that leaves out something it renewed leaves a version the worker
	// cannot name again, so the hold is gone: the same verdict a refused
	// renewal reaches, and the same error, so the run loop drops this task and
	// takes another instead of ending the process.
	for name, bad := range map[string]*entroq.ModifyResponse{
		"missing a set":    {ChangedTasks: resp.ChangedTasks, ChangedSets: resp.ChangedSets[:1]},
		"missing the task": {ChangedSets: resp.ChangedSets},
		"naming another task": {
			ChangedTasks: []*entroq.Task{{ID: "other", Version: 4}},
			ChangedSets:  resp.ChangedSets,
		},
	} {
		_, err := h.renewed(bad)
		depErr, ok := entroq.AsDependency(err)
		if !ok {
			t.Errorf("Response %s: want a dependency error, got %v", name, err)
			continue
		}
		if len(depErr.Depends) != 1 || depErr.Depends[0].ID != task.ID {
			t.Errorf("Response %s: want task %q among the depends, got %v", name, task.ID, depErr.Depends)
		}
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
		WithTakeDocs(func(context.Context, entroq.Reader, *TaskRun[string]) (*TakeResult, error) {
			time.Sleep(lease * 7 / 10)
			return Take(entroq.ClaimKey("ns", "k")), nil
		}),
		WithDoModify(func(_ context.Context, _ entroq.Reader, tRun *TaskRun[string]) (*Result, error) {
			task := tRun.Task
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
		WithTakeDocs(func(context.Context, entroq.Reader, *TaskRun[string]) (*TakeResult, error) {
			// The lease and claimant are the worker's: these are ignored.
			return Take(entroq.ClaimKey("ns", "a"), entroq.ClaimKey("ns", "b").WithoutMembers(),
				entroq.ClaimingSetsFor(time.Hour), entroq.ClaimingSetsAs("someone else")), nil
		}),
		WithDoModify(func(_ context.Context, _ entroq.Reader, tRun *TaskRun[string]) (*Result, error) {
			task, sets := tRun.Task, tRun.Sets
			got <- seen{task.At, sets[0].At, sets[1].At, []string{sets[0].Claimant, sets[1].Claimant}}
			return Modify(task.Delete()), nil
		}),
	)
	runCtx, runCancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- w.Run(runCtx, Watching("q"), AsClaimant(holder), WithLease(time.Minute)) }()

	s := <-got
	if !s.a.Equal(s.task) || !s.b.Equal(s.task) {
		t.Errorf("Sets held until %v and %v, want both until the task's arrival %v", s.a, s.b, s.task)
	}
	// Held by the RUN, which is its own consumer: the connection's claimant is
	// a different one, and a set held by the Run excludes it like any other.
	for _, h := range s.holders {
		if h != holder {
			t.Errorf("Set held by %q, want the Run's own claimant %q", h, holder)
		}
	}
	runCancel()
	if err := <-errCh; err != nil {
		t.Errorf("Run: %v", err)
	}
}

// TestQuarantineAtClaimLimit checks that a handler's unknown error on the
// task's last allowed claim moves the task to its error queue with that error,
// from DoWork and from TakeDocs alike, and that the worker still stops; and
// that below the limit nothing is recorded.
func TestQuarantineAtClaimLimit(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name      string
		maxClaims int32
		takeFails bool
		wantMoved bool
	}{
		{"DoWork at the limit", 1, false, true},
		{"TakeDocs at the limit", 1, true, true},
		{"below the limit", 3, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

			w := New[string](client,
				WithTakeDocs(func(context.Context, entroq.Reader, *TaskRun[string]) (*TakeResult, error) {
					if tc.takeFails {
						return nil, boom
					}
					return Take(entroq.ClaimKey("ns", "k")), nil
				}),
				WithDoWork(func(context.Context, entroq.Reader, *TaskRun[string]) error {
					return boom
				}),
			)
			if err := w.Run(ctx, Watching("q"), WithMaxClaims(tc.maxClaims), WithLease(time.Minute)); !errors.Is(err, boom) {
				t.Fatalf("Run: want it to stop with the handler's error, got %v", err)
			}

			moved, err := client.Tasks(ctx, "q/err")
			if err != nil {
				t.Fatalf("Tasks: %v", err)
			}
			if !tc.wantMoved {
				if len(moved) != 0 {
					t.Errorf("Below the limit: want nothing moved, got %v", moved)
				}
				return
			}
			if len(moved) != 1 || !strings.Contains(moved[0].Err, "boom") || !strings.Contains(moved[0].Err, "claim limit") {
				t.Fatalf("At the limit: want the task moved with the error, got %v", moved)
			}
			// Its doc set was released with it.
			if _, err := client.ClaimDocs(ctx, entroq.ClaimKey("ns", "k"), entroq.ClaimingSetsAs("other")); err != nil {
				t.Errorf("Set after the move: want it free, got %v", err)
			}
		})
	}
}

// TestTakeDocsSentinel checks that a sentinel from TakeDocs acts on the task as
// one from DoWork does: a move moves it, and the worker goes on.
func TestTakeDocsSentinel(t *testing.T) {
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
	w := New[string](client,
		WithTakeDocs(func(context.Context, entroq.Reader, *TaskRun[string]) (*TakeResult, error) {
			return nil, MoveErrorf("no docs for this one")
		}),
		WithDoWork(func(context.Context, entroq.Reader, *TaskRun[string]) error {
			t.Error("DoWork ran for a task TakeDocs moved")
			return nil
		}),
	)
	runCtx, runCancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- w.Run(runCtx, Watching("q")) }()
	waitForTasks(ctx, t, client, "q/err", 1)
	runCancel()
	if err := <-errCh; err != nil {
		t.Errorf("Run: want it to go on after the move, got %v", err)
	}
}

// TestDocContentionDelay checks that a task whose doc set someone else holds
// is retried after the contention delay, with up to a quarter more, and that
// the retry counts as an attempt.
func TestDocContentionDelay(t *testing.T) {
	const delay = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()
	if _, err := client.ClaimDocs(ctx, entroq.ClaimKey("ns", "busy"), entroq.ClaimingSetsAs("holder"), entroq.ClaimingSetsFor(time.Hour)); err != nil {
		t.Fatalf("Holder claim: %v", err)
	}
	if _, err := client.Modify(ctx, entroq.InsertingInto("q")); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	w := New[string](client,
		WithTakeDocs(func(context.Context, entroq.Reader, *TaskRun[string]) (*TakeResult, error) {
			return Take(entroq.ClaimKey("ns", "busy")), nil
		}),
		WithDoWork(func(context.Context, entroq.Reader, *TaskRun[string]) error {
			t.Error("DoWork ran without its doc set")
			return nil
		}),
	)
	runCtx, runCancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	before := time.Now()
	go func() { errCh <- w.Run(runCtx, Watching("q"), WithDocContentionDelay(delay)) }()

	var task *entroq.Task
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		tasks, err := client.Tasks(ctx, "q")
		if err != nil {
			t.Fatalf("Tasks: %v", err)
		}
		if len(tasks) == 1 && tasks[0].Attempt == 1 {
			task = tasks[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Task not retried after contention: %v", tasks)
		}
	}
	runCancel()
	if err := <-errCh; err != nil {
		t.Errorf("Run: %v", err)
	}
	if wait := task.At.Sub(before); wait < delay || wait > delay*5/4+time.Second {
		t.Errorf("Retry after contention: want it ready in %v to %v, got %v", delay, delay*5/4, wait)
	}
}

func TestContentionDelayJitter(t *testing.T) {
	ro := &runOpt{baseRetryDelay: time.Second}
	for range 100 {
		if d := ro.contentionDelay(); d < time.Second || d > time.Second*5/4 {
			t.Fatalf("Default contention delay: want 1s to 1.25s, got %v", d)
		}
	}
	ro.docContentionDelay = time.Minute
	for range 100 {
		if d := ro.contentionDelay(); d < time.Minute || d > time.Minute*5/4 {
			t.Fatalf("Contention delay: want 1m to 1m15s, got %v", d)
		}
	}
}

// TestLostClaimGoesOn checks that a lost claim ends that task, not the
// worker, whatever the handler returns once its context is canceled: the
// worker goes on to the next task, and the lost task is not quarantined, even
// at its claim limit.
func TestLostClaimGoesOn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result func(ctx context.Context) error
	}{
		{"handler returns the cancellation", func(ctx context.Context) error { return ctx.Err() }},
		{"handler returns its own error", func(context.Context) error { return errors.New("write failed") }},
		{"handler returns a retry", func(context.Context) error { return RetryErrorf("again") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client, err := entroq.New(ctx, eqmem.Opener())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer client.Close()
			resp, err := client.Modify(ctx, entroq.InsertingInto("q", entroq.WithValue("lost")))
			if err != nil {
				t.Fatalf("Insert: %v", err)
			}
			lostID := resp.InsertedTasks[0].ID

			working := make(chan *entroq.Task, 1)
			second := make(chan bool, 1)
			w := New[string](client, WithDoWork(func(ctx context.Context, _ entroq.Reader, tRun *TaskRun[string]) error {
				task, v := tRun.Task, tRun.Value
				if v != "lost" {
					second <- true
					return nil
				}
				working <- task
				<-ctx.Done() // the claim is lost underneath
				return tc.result(ctx)
			}))
			runCtx, runCancel := context.WithCancel(ctx)
			errCh := make(chan error, 1)
			go func() {
				errCh <- w.Run(runCtx, Watching("q"), AsClaimant(holder), WithLease(200*time.Millisecond), WithMaxClaims(1))
			}()

			task := <-working
			// The task changes underneath the worker: the version it holds is
			// gone, so its next renewal finds the claim lost.
			//
			// The change comes from the consumer HOLDING the task, which is
			// the Run and not the connection. Every Run is its own consumer
			// now, so the connection's own claimant is a different one and is
			// excluded from a held task -- which is the point of that, and
			// means this has to name the holder to move the task at all.
			if _, err := client.As(holder).Modify(ctx, task.Change(entroq.ValueTo("taken"), entroq.ArrivalTimeBy(time.Hour))); err != nil {
				t.Fatalf("Change underneath: %v", err)
			}
			if _, err := client.Modify(ctx, entroq.InsertingInto("q", entroq.WithValue("next"))); err != nil {
				t.Fatalf("Insert next: %v", err)
			}
			select {
			case <-second:
			case err := <-errCh:
				t.Fatalf("Run stopped after the lost claim: %v", err)
			case <-ctx.Done():
				t.Fatal("Worker did not go on to the next task")
			}
			runCancel()
			if err := <-errCh; err != nil {
				t.Errorf("Run: %v", err)
			}
			if moved, _ := client.Tasks(ctx, "q/err"); len(moved) != 0 {
				t.Errorf("Lost task quarantined: %v", moved)
			}
			if tasks, _ := client.Tasks(ctx, "q", entroq.WithTaskID(lostID)); len(tasks) != 1 || string(tasks[0].Value) != `"taken"` {
				t.Errorf("Lost task: want it as the change left it, got %v", tasks)
			}
		})
	}
}

// holder names the consumer a Run holds its task as, where a test has to act
// as that consumer to move the task. A Run would otherwise pick its own name
// and nothing outside it could say what that was.
const holder = "test-holder"

// TestLostClaimFatalStops checks that a handler's FatalError still stops the
// worker when its claim was lost: handlers keep that control.
func TestLostClaimFatalStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()
	if _, err := client.Modify(ctx, entroq.InsertingInto("q")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	working := make(chan *entroq.Task, 1)
	w := New[string](client, WithDoWork(func(ctx context.Context, _ entroq.Reader, tRun *TaskRun[string]) error {
		task := tRun.Task
		working <- task
		<-ctx.Done()
		return FatalErrorf("cannot go on")
	}))
	errCh := make(chan error, 1)
	go func() {
		errCh <- w.Run(ctx, Watching("q"), AsClaimant(holder), WithLease(200*time.Millisecond))
	}()
	task := <-working
	// As the holder: a Run is its own consumer, so the connection cannot
	// touch a task the Run holds.
	if _, err := client.As(holder).Modify(ctx, task.Change(entroq.ArrivalTimeBy(time.Hour))); err != nil {
		t.Fatalf("Change underneath: %v", err)
	}
	if _, ok := AsFatal(<-errCh); !ok {
		t.Error("Run after a lost claim and a FatalError: want it to stop with the fatal error")
	}
}

// TestUndecodableValueMoves checks that a task whose value does not decode
// into the worker's type moves to its error queue, saying why, and that the
// worker goes on to the next task.
func TestUndecodableValueMoves(t *testing.T) {
	type job struct{ N int }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()
	if _, err := client.Modify(ctx, entroq.InsertingInto("q", entroq.WithValue("not an object"))); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	done := make(chan int, 1)
	w := New[job](client, WithDoModify(func(_ context.Context, _ entroq.Reader, tRun *TaskRun[job]) (*Result, error) {
		task, v := tRun.Task, tRun.Value
		done <- v.N
		return Modify(task.Delete()), nil
	}))
	runCtx, runCancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- w.Run(runCtx, Watching("q")) }()

	waitForTasks(ctx, t, client, "q/err", 1)
	moved, err := client.Tasks(ctx, "q/err")
	if err != nil || len(moved) != 1 || !strings.Contains(moved[0].Err, "does not decode") || !strings.Contains(moved[0].Err, "job") {
		t.Fatalf("Moved task: want it in the error queue saying it does not decode as job, got %v, %v", moved, err)
	}
	if _, err := client.Modify(ctx, entroq.InsertingInto("q", entroq.WithValue(job{N: 7}))); err != nil {
		t.Fatalf("Insert next: %v", err)
	}
	select {
	case n := <-done:
		if n != 7 {
			t.Errorf("Next task: got value %d, want 7", n)
		}
	case err := <-errCh:
		t.Fatalf("Run stopped after the undecodable task: %v", err)
	case <-ctx.Done():
		t.Fatal("Worker did not go on to the next task")
	}
	runCancel()
	if err := <-errCh; err != nil {
		t.Errorf("Run: %v", err)
	}
}

// leaseFloorBackend grants at least floor, however short a lease the claim
// asked for, the way a service's lease floor does. Embedding the interface
// leaves every other method alone, so only claiming behaves differently.
type leaseFloorBackend struct {
	entroq.Backend
	floor time.Duration
}

func (b *leaseFloorBackend) raise(cq *entroq.ClaimQuery) *entroq.ClaimQuery {
	if cq.Duration >= b.floor {
		return cq
	}
	raised := *cq
	raised.Duration = b.floor
	return &raised
}

func (b *leaseFloorBackend) Claim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	return b.Backend.Claim(ctx, b.raise(cq))
}

func (b *leaseFloorBackend) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	return b.Backend.TryClaim(ctx, b.raise(cq))
}

func leaseFloorOpener(inner entroq.BackendOpener, floor time.Duration) entroq.BackendOpener {
	return func(ctx context.Context) (entroq.Backend, error) {
		b, err := inner(ctx)
		if err != nil {
			return nil, err
		}
		return &leaseFloorBackend{Backend: b, floor: floor}, nil
	}
}

// TestRenewalHoldsTheGrantedLease pins the half of the granted-lease rule that
// only shows when a claim is clamped: the hold a renewal asks for.
//
// The claim requests far less than the floor, so the lease it is GRANTED is
// much longer than the one it asked for. A renewal that asked for the original
// request would shorten the hold to less than the cadence and the task would
// fall free between renewals, which is exactly what happened before
// doWhileRenewing stopped being given a lease at all. Renewing for the granted
// lease instead keeps every grant equal to the last, which is also why
// computing the cadence once is exact rather than merely convenient.
//
// Run against a default service this would pass either way, because the
// default lease equals the default floor and the two values coincide. The
// clamp is what makes the assertion mean anything.
func TestRenewalHoldsTheGrantedLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const (
		asked = 50 * time.Millisecond
		floor = 2 * time.Second
	)
	client, err := entroq.New(ctx, leaseFloorOpener(eqmem.Opener(), floor))
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer client.Close()

	const queue = "renewal_granted_lease"
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	task, err := client.Claim(ctx, entroq.From(queue), entroq.ClaimFor(asked))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	granted := grantedLease(task)
	if granted < floor {
		t.Fatalf("claim granted %v, want at least the floor %v: the backend did not clamp, so this test proves nothing", granted, floor)
	}

	// Long enough for several renewals, and far longer than the lease the
	// claim asked for.
	working := 3 * renewInterval(task)
	renewErr, err := doWhileRenewing(ctx, client, 0, held{task: task}, nil, func(ctx context.Context, stop finalizeRenew) error {
		select {
		case <-ctx.Done():
			return fmt.Errorf("work: %w", ctx.Err())
		case <-time.After(working):
		}
		stable := stop()
		if stable.task.Version <= task.Version {
			t.Errorf("no renewal happened in %v: version still %d", working, stable.task.Version)
		}
		// Each renewal must grant what the last one did. A renewal for the
		// asked-for lease would show up here as a hold collapsed to ~asked.
		if got := grantedLease(stable.task); got != granted {
			t.Errorf("renewed hold is %v, want the granted lease %v (the claim asked for %v)", got, granted, asked)
		}
		return nil
	})
	if renewErr != nil || err != nil {
		t.Fatalf("doWhileRenewing: renew %v, work %v", renewErr, err)
	}
}
