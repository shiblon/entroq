package eqmr_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shiblon/entroq/pkg/eqmr"
	"github.com/shiblon/entroq/pkg/shardname"
	"github.com/shiblon/entroq/pkg/worker"
)

// writeFiles writes each file's content under root.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("make dir for %q: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
}

// under returns names joined to root.
func under(root string, names ...string) []string {
	paths := make([]string, len(names))
	for i, name := range names {
		paths[i] = filepath.Join(root, name)
	}
	return paths
}

// readSplits reads every record of src, split by split.
func readSplits(ctx context.Context, t *testing.T, src eqmr.Source) [][]string {
	t.Helper()
	splits, err := src.Splits(ctx)
	if err != nil {
		t.Fatalf("splits: %v", err)
	}
	var got [][]string
	for _, split := range splits {
		records, err := src.Open(ctx, split)
		if err != nil {
			t.Fatalf("open %s: %v", split, err)
		}
		var kvs []string
		for {
			kv, err := records.Next(ctx)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("next in %s: %v", split, err)
			}
			kvs = append(kvs, kv.Key+"="+kv.Value)
		}
		if err := records.Close(); err != nil {
			t.Fatalf("close %s: %v", split, err)
		}
		got = append(got, kvs)
	}
	return got
}

// startRun runs a controller, mappers, and reducers for ctrl's run until the
// test ends.
func startRun(ctx context.Context, t *testing.T, ctrl *eqmr.Controller, mapFn eqmr.Mapper, reduceFn eqmr.Reducer) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	start := func(run func(context.Context) error) {
		wg.Add(1)
		go func() { defer wg.Done(); _ = run(ctx) }()
	}
	start(ctrl.RunController)
	for range 2 {
		start(func(ctx context.Context) error { return ctrl.RunMapper(ctx, mapFn) })
		start(func(ctx context.Context) error { return ctrl.RunReducer(ctx, reduceFn) })
	}
}

func TestFileSourceReadsLines(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"a.txt":     "one\r\ntwo\n\nfour",
		"sub/b.txt": "only\n",
		"empty.txt": "",
	})
	src, err := eqmr.NewFileSource("in", eqmr.WithFiles(under(root, "sub/b.txt", "a.txt", "empty.txt")...))
	if err != nil {
		t.Fatalf("new file source: %v", err)
	}
	got := readSplits(ctx, t, src)
	a, b := filepath.Join(root, "a.txt"), filepath.Join(root, "sub/b.txt")
	want := [][]string{
		{b + ":1=only"},
		{a + ":1=one", a + ":2=two", a + ":3=", a + ":4=four"},
		nil,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d splits, want one per file: %q", len(got), got)
	}
	for i := range want {
		if strings.Join(got[i], "|") != strings.Join(want[i], "|") {
			t.Errorf("split %d is %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFileSourceReadsTSV(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"good.tsv": "k1\tv1\nk2\tv\t2\n",
		"bad.tsv":  "k1\tv1\nno tab\n",
	})
	src, err := eqmr.NewFileSource("in", eqmr.WithFiles(under(root, "good.tsv")...), eqmr.WithFormat(eqmr.TSV))
	if err != nil {
		t.Fatalf("new file source: %v", err)
	}
	if got := strings.Join(readSplits(ctx, t, src)[0], "|"); got != "k1=v1|k2=v\t2" {
		t.Errorf("read %q, want key and value split at the first tab", got)
	}

	bad, err := eqmr.NewFileSource("in", eqmr.WithFiles(under(root, "bad.tsv")...), eqmr.WithFormat(eqmr.TSV))
	if err != nil {
		t.Fatalf("new file source: %v", err)
	}
	splits, err := bad.Splits(ctx)
	if err != nil {
		t.Fatalf("splits: %v", err)
	}
	records, err := bad.Open(ctx, splits[0])
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer records.Close()
	if _, err := records.Next(ctx); err != nil {
		t.Fatalf("first line: %v", err)
	}
	if _, err := records.Next(ctx); !isMove(err) {
		t.Errorf("line with no tab: got %v, want a MoveError", err)
	}
}

func isMove(err error) bool {
	_, ok := worker.AsMove(err)
	return ok
}

func TestFileSourceRejectsBadLists(t *testing.T) {
	root := t.TempDir()
	for _, files := range [][]string{{"a.txt"}, {"../a.txt"}, under(root, "a", "a")} {
		if _, err := eqmr.NewFileSource("in", eqmr.WithFiles(files...)); err == nil {
			t.Errorf("NewFileSource(%q) succeeded, want an error", files)
		}
	}
	if _, err := eqmr.NewFileSource("in", eqmr.WithFormat(eqmr.FileFormat{})); err == nil {
		t.Error("NewFileSource with no format succeeded, want an error")
	}
}

// TestFileSourceRun counts words in a sharded file set, with intermediates in
// files too, so no input or intermediate data passes through EntroQ.
func TestFileSourceRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq := newClient(ctx, t)
	root := t.TempDir()

	input, histogram := wordCountFixture(t, 40, 1500, 12)
	files, err := shardname.Expand("corpus/words@5")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	contents := make(map[string]string)
	for i, kv := range input {
		contents[files[i%len(files)]] += kv.Value + "\n"
	}
	writeFiles(t, root, contents)

	store, err := eqmr.NewFileStore("scratch", filepath.Join(root, "scratch"))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	src, err := eqmr.NewFileSource("corpus", eqmr.WithFiles(under(root, files...)...))
	if err != nil {
		t.Fatalf("new file source: %v", err)
	}
	launcher, err := eqmr.New(eq, testPrefix(), eqmr.WithReduceShards(3), eqmr.WithIntermediateStore(store), eqmr.WithSource(src))
	if err != nil {
		t.Fatalf("new launcher: %v", err)
	}
	if err := launcher.SetupFrom(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// A retry of the same setup is recognized.
	if err := launcher.SetupFrom(ctx); err != nil {
		t.Fatalf("retried setup: %v", err)
	}

	// A mapper names the source without its file list.
	mapperSrc, err := eqmr.NewFileSource("corpus")
	if err != nil {
		t.Fatalf("new mapper source: %v", err)
	}
	ctrl, err := eqmr.New(eq, launcher.Prefix(), eqmr.WithIntermediateStore(store), eqmr.WithSource(mapperSrc))
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	p, err := ctrl.Progress(ctx)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if p.Map.Total != len(files) {
		t.Errorf("run has %d splits, want one per file (%d)", p.Map.Total, len(files))
	}

	startRun(ctx, t, ctrl, eqmr.WordCountMapper, eqmr.SumReducer)
	if err := ctrl.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	results, err := ctrl.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if got, want := strings.Join(kvStrings(results), ","), strings.Join(expectedFromHistogram(histogram), ","); got != want {
		t.Fatalf("results %v, want %v", got, want)
	}
	requireCollected(ctx, t, eq, ctrl)
}

func TestSetupFromRejects(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.txt": "a\n"})

	src, err := eqmr.NewFileSource("in", eqmr.WithFiles(under(root, "a.txt")...))
	if err != nil {
		t.Fatalf("new file source: %v", err)
	}
	missing, err := eqmr.NewFileSource("in", eqmr.WithFiles(under(root, "a.txt", "gone.txt")...))
	if err != nil {
		t.Fatalf("new file source: %v", err)
	}
	empty, err := eqmr.NewFileSource("in")
	if err != nil {
		t.Fatalf("new file source: %v", err)
	}
	elsewhere, err := eqmr.NewFileSource("in", eqmr.WithFiles(under(root, "a.txt")...), eqmr.WithFormat(eqmr.TSV))
	if err != nil {
		t.Fatalf("new file source: %v", err)
	}

	sharded, err := eqmr.New(eq, testPrefix(), append(testOpts(2, 2), eqmr.WithSource(src))...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if err := sharded.SetupFrom(ctx); err == nil || !strings.Contains(err.Error(), "MapShards") {
		t.Errorf("SetupFrom with MapShards set: got %v, want an error naming MapShards", err)
	}

	prefix := testPrefix()
	setupFrom := func(opts ...eqmr.Option) error {
		ctrl, err := eqmr.New(eq, prefix, append(opts, eqmr.WithReduceShards(2))...)
		if err != nil {
			t.Fatalf("new controller: %v", err)
		}
		return ctrl.SetupFrom(ctx)
	}
	if err := setupFrom(); err == nil || !strings.Contains(err.Error(), "WithSource") {
		t.Errorf("SetupFrom with no source: got %v, want an error naming WithSource", err)
	}
	if err := setupFrom(eqmr.WithSource(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("SetupFrom with a missing file: got %v, want not-exist", err)
	}
	if err := setupFrom(eqmr.WithSource(empty)); err == nil {
		t.Error("SetupFrom with no files succeeded, want an error")
	}
	if err := setupFrom(eqmr.WithSource(src)); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := setupFrom(eqmr.WithSource(elsewhere)); err == nil || !strings.Contains(err.Error(), "different run") {
		t.Errorf("SetupFrom of another source on the same prefix: got %v, want a different-run error", err)
	}
}

// TestSourceFailuresFailRun covers a mapper that was not given the run's
// source or was given it with another format, and an input file removed after
// Setup. Neither can succeed on a
// retry, so each fails the run with a reason.
func TestSourceFailuresFailRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before func(root string, ctrlOpts *[]eqmr.Option, src eqmr.Source)
		reason string
	}{
		{
			name:   "unconfigured",
			before: func(string, *[]eqmr.Option, eqmr.Source) {},
			reason: "not configured",
		},
		{
			name: "other format",
			before: func(_ string, opts *[]eqmr.Option, _ eqmr.Source) {
				tsv, err := eqmr.NewFileSource("in", eqmr.WithFormat(eqmr.TSV))
				if err != nil {
					panic(err)
				}
				*opts = append(*opts, eqmr.WithSource(tsv))
			},
			reason: "identity format=lines",
		},
		{
			name: "removed",
			before: func(root string, opts *[]eqmr.Option, src eqmr.Source) {
				*opts = append(*opts, eqmr.WithSource(src))
				os.Remove(filepath.Join(root, "b.txt"))
			},
			reason: "b.txt",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			eq := newClient(ctx, t)
			root := t.TempDir()
			writeFiles(t, root, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})

			src, err := eqmr.NewFileSource("in", eqmr.WithFiles(under(root, "a.txt", "b.txt")...))
			if err != nil {
				t.Fatalf("new file source: %v", err)
			}
			launcher, err := eqmr.New(eq, testPrefix(), eqmr.WithReduceShards(2), eqmr.WithSource(src))
			if err != nil {
				t.Fatalf("new launcher: %v", err)
			}
			if err := launcher.SetupFrom(ctx); err != nil {
				t.Fatalf("setup: %v", err)
			}

			var opts []eqmr.Option
			tc.before(root, &opts, src)
			ctrl, err := eqmr.New(eq, launcher.Prefix(), opts...)
			if err != nil {
				t.Fatalf("new controller: %v", err)
			}
			startRun(ctx, t, ctrl, eqmr.WordCountMapper, eqmr.SumReducer)
			if err := ctrl.Wait(ctx); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("wait: got %v, want a failed run naming %q", err, tc.reason)
			}
		})
	}
}
