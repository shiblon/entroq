package eqmr_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/eqmr"
)

// TestCommitWhoseReplyIsLost covers a controller that commits a partition's
// output to the sink and dies before recording it. Every reduce task is
// duplicated, so a second finish for the same partition can be judged while
// the first is still pending; whichever way the two land, the committed output
// must survive, and only uncommitted attempts may be swept.
func TestCommitWhoseReplyIsLost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq := newClient(ctx, t)

	sink := newMemSink("mem")
	sink.failCommitOnce = true
	ctrl, err := eqmr.New(eq, testPrefix(), append(testOpts(2, 1), eqmr.WithSink(sink))...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	input, histogram := wordCountFixture(t, 20, 200, 4)
	if err := ctrl.Setup(ctx, input); err != nil {
		t.Fatalf("setup: %v", err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = ctrl.RunController(runCtx) }()
	go func() { _ = ctrl.RunMapper(runCtx, eqmr.WordCountMapper) }()
	waitForPhase(ctx, t, ctrl, eqmr.PhaseReduce)

	if n := duplicateTasks(ctx, t, eq, ctrl.ReduceQ()); n != 1 {
		t.Fatalf("duplicated %d reduce tasks, want 1", n)
	}
	for range 2 {
		go func() { _ = ctrl.RunReducer(runCtx, eqmr.SumReducer) }()
	}
	if err := ctrl.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}

	want := strings.Join(expectedFromHistogram(histogram), ",")
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := strings.Join(sink.output(), ",")
		if got != want {
			t.Fatalf("committed output %q, want %q", got, want)
		}
		sink.mu.Lock()
		leftover := len(sink.staged) - len(sink.committed)
		sink.mu.Unlock()
		if leftover == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d uncommitted attempts never swept", leftover)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestControllerWithoutSinkFailsRun starts a controller that was not given the
// sink its run names. It cannot ever finish the run, so the run fails at once
// with a reason naming the sink, and the controller exits.
func TestControllerWithoutSinkFailsRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newClient(ctx, t)

	prefix := testPrefix()
	launcher, err := eqmr.New(eq, prefix, append(testOpts(2, 2), eqmr.WithSink(newMemSink("elsewhere")))...)
	if err != nil {
		t.Fatalf("new launcher: %v", err)
	}
	input, _ := wordCountFixture(t, 10, 50, 2)
	if err := launcher.Setup(ctx, input); err != nil {
		t.Fatalf("setup: %v", err)
	}

	misconfigured, err := eqmr.New(eq, prefix, testOpts(2, 2)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if err := misconfigured.RunController(ctx); err == nil || !strings.Contains(err.Error(), "elsewhere") {
		t.Fatalf("controller without the run's sink: got %v, want an exit naming the sink", err)
	}
	phase, reason, err := launcher.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if phase != eqmr.PhaseFailed || !strings.Contains(reason, "elsewhere") {
		t.Errorf("run is %q (%s), want failed naming the sink", phase, reason)
	}
}

// TestFailedRunIsSwept fails a run partway and requires its intermediate data
// to be swept: once a run has ended, its intermediates are useless however it
// ended, and potentially large.
func TestFailedRunIsSwept(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq := newClient(ctx, t)
	root := t.TempDir()

	store, err := eqmr.NewFileStore("scratch", root)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	ctrl, err := eqmr.New(eq, testPrefix(), append(testOpts(3, 2), eqmr.WithIntermediateStore(store))...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	input, _ := wordCountFixture(t, 20, 300, 6)
	if err := ctrl.Setup(ctx, input); err != nil {
		t.Fatalf("setup: %v", err)
	}
	tasks, err := eq.Tasks(ctx, ctrl.MapQ())
	if err != nil || len(tasks) == 0 {
		t.Fatalf("list map tasks: %d, %v", len(tasks), err)
	}
	mapOnly(ctx, t, eq, ctrl)
	if len(runDirs(t, root)) != 1 {
		t.Fatal("mappers wrote no run directory")
	}

	// A quarantined copy of a map task whose split is still unaccepted is a
	// genuine failure. No reducers run, so the run cannot finish first.
	if _, err := eq.Modify(ctx, entroq.InsertingInto(ctrl.MapQ()+"/err",
		entroq.WithRawValue(tasks[0].Value),
	)); err != nil {
		t.Fatalf("seed quarantined task: %v", err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = ctrl.RunController(runCtx) }()
	if err := ctrl.Wait(ctx); err == nil {
		t.Fatal("run did not fail")
	}

	deadline := time.Now().Add(5 * time.Second)
	for len(runDirs(t, root)) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("failed run's directory survived: %v", runDirs(t, root))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCorruptRunFailsRunPromptly damages a stored intermediate run between the
// map and reduce phases. Reading it again would read the same wrong bytes, so
// the reducer's task is quarantined on the first read, not retried, and the run
// fails naming the corruption.
func TestCorruptRunFailsRunPromptly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	eq := newClient(ctx, t)
	root := t.TempDir()

	store, err := eqmr.NewFileStore("scratch", root)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	ctrl, err := eqmr.New(eq, testPrefix(), append(testOpts(2, 1), eqmr.WithIntermediateStore(store))...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	input, _ := wordCountFixture(t, 20, 200, 4)
	if err := ctrl.Setup(ctx, input); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mapOnly(ctx, t, eq, ctrl)

	objects, err := filepath.Glob(filepath.Join(root, "run-*", "run-*"))
	if err != nil || len(objects) == 0 {
		t.Fatalf("find stored runs: %v, %v", objects, err)
	}
	data, err := os.ReadFile(objects[0])
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(objects[0], data, 0o644); err != nil {
		t.Fatalf("damage run: %v", err)
	}

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = ctrl.RunController(runCtx) }()
	go func() { _ = ctrl.RunReducer(runCtx, eqmr.SumReducer) }()

	began := time.Now()
	err = ctrl.Wait(ctx)
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("run over a corrupt intermediate: got %v, want a failure naming the corruption", err)
	}
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Errorf("failing took %v; a corrupt run should not be retried", elapsed)
	}
}

// TestUnstorableOutputFailsRunPromptly has a reducer return a value the
// document sink can never store. The sink says so, permanently, so the run
// fails at once with the reason instead of retrying a write that cannot work.
func TestUnstorableOutputFailsRunPromptly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	eq := newClient(ctx, t)

	ctrl, err := eqmr.New(eq, testPrefix(), testOpts(2, 1)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	input, _ := wordCountFixture(t, 5, 50, 2)
	nul := func(context.Context, eqmr.ReducerInput) (string, error) { return "bad\x00value", nil }

	began := time.Now()
	err = ctrl.Run(ctx, input, eqmr.WordCountMapper, nul, eqmr.RunOptions{Mappers: 1, Reducers: 1})
	if err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("run with unstorable output: got %v, want a failure naming the NUL", err)
	}
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Errorf("failing took %v; output that can never be stored should not be retried", elapsed)
	}
}
