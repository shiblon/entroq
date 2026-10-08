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
	"github.com/shiblon/entroq/pkg/eqmr/eqmrtest"
)

// TestFileStoreRun runs a whole MapReduce with its intermediate data in files.
// No intermediate data is ever written to a doc, and the run's directory is
// swept away once the run is done.
func TestFileStoreRun(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)
	root := t.TempDir()

	store, err := eqmr.NewFileStore("scratch", root)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	ctrl, err := eqmr.New(eq, testPrefix(), append(testOpts(4, 3), eqmr.WithIntermediateStore(store))...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	input, histogram := wordCountFixture(t, 60, 2000, 12)

	// Watch for intermediate docs while the run is live, not only afterwards.
	stop := make(chan struct{})
	sawDocRuns := make(chan bool, 1)
	go func() {
		saw := false
		defer func() { sawDocRuns <- saw }()
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			docs, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: ctrl.DocNS(), KeyStart: "run/", KeyEnd: "run0", OmitValues: true})
			if err == nil && len(docs) > 0 {
				saw = true
			}
		}
	}()

	if err := ctrl.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
		Mappers: 3, Reducers: 2, IntermediateRunBytes: 2048,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	close(stop)
	if <-sawDocRuns {
		t.Error("intermediate runs were written to docs despite the file store")
	}

	results, err := ctrl.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if got, want := strings.Join(kvStrings(results), ","), strings.Join(expectedFromHistogram(histogram), ","); got != want {
		t.Fatalf("results %v, want %v", got, want)
	}
	requireCollected(ctx, t, eq, ctrl)
	if dirs := runDirs(t, root); len(dirs) != 0 {
		t.Errorf("run directories remain after the run: %v", dirs)
	}
}

// TestFileStoreSweepsOrphans plants a file that no finish task names, as a
// mapper that crashed after writing would leave, and requires the sweep at the
// end of the run to remove it.
func TestFileStoreSweepsOrphans(t *testing.T) {
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
	input, histogram := wordCountFixture(t, 20, 300, 6)
	if err := ctrl.Setup(ctx, input); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mapOnly(ctx, t, eq, ctrl)

	dirs := runDirs(t, root)
	if len(dirs) != 1 {
		t.Fatalf("got run directories %v, want exactly one", dirs)
	}
	orphan := filepath.Join(dirs[0], "run-orphan")
	if err := os.WriteFile(orphan, []byte("written by a mapper that died"), 0o644); err != nil {
		t.Fatalf("plant orphan: %v", err)
	}

	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	go func() { _ = ctrl.RunController(runCtx) }()
	go func() { _ = ctrl.RunReducer(runCtx, eqmr.SumReducer) }()
	if err := ctrl.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	results, err := ctrl.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if err := eqmrtest.VerifyHistogram(results, histogram); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := os.Stat(orphan)
		if os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphan survived the run: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
