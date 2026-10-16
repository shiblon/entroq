package eqmr_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq/pkg/eqmr"
	"github.com/shiblon/entroq/pkg/shardname"
)

// readShards reads every record of the shards of base@n, in order.
func readShards(ctx context.Context, t *testing.T, base string, n int, format eqmr.FileFormat) []string {
	t.Helper()
	files, err := shardname.Expand(fmt.Sprintf("%s@%d", base, n))
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	src, err := eqmr.NewFileSource("check", eqmr.WithFiles(files...), eqmr.WithFormat(format))
	if err != nil {
		t.Fatalf("new file source: %v", err)
	}
	var all []string
	for _, split := range readSplits(ctx, t, src) {
		all = append(all, split...)
	}
	return all
}

// waitSwept waits for the sink's staging directory under dir to be removed.
func waitSwept(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		staged, _ := filepath.Glob(filepath.Join(dir, ".*.eqmr", "staged"))
		if len(staged) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("staging directory %v never swept", staged)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestFileSinkChainsRuns counts words into TSV shards, then reads those shards
// back as the next run's input, counting words by their count.
func TestFileSinkChainsRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq := newClient(ctx, t)
	dir := t.TempDir()
	base := filepath.Join(dir, "counts")

	sink, err := eqmr.NewFileSink("counts", base, eqmr.TSV)
	if err != nil {
		t.Fatalf("new file sink: %v", err)
	}
	// More partitions than words, so some partitions are empty.
	const parts = 8
	input := []*eqmr.KV{eqmr.NewKV("", "a b a c"), eqmr.NewKV("", "b a d")}
	first, err := eqmr.New(eq, testPrefix(), append(testOpts(2, parts), eqmr.WithSink(sink))...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if err := first.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{Mappers: 2, Reducers: 2}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	got := readShards(ctx, t, base, parts, eqmr.TSV)
	slices.Sort(got)
	if want := []string{"a=3", "b=2", "c=1", "d=1"}; !slices.Equal(got, want) {
		t.Fatalf("output %q, want %q", got, want)
	}
	raw, err := os.ReadFile(base + ".manifest")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Run   string   `json:"run"`
		Files []string `json:"files"`
		Empty []int    `json:"empty"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if manifest.Run != first.Prefix() || len(manifest.Files) != parts || len(manifest.Empty) < parts-4 {
		t.Errorf("manifest %s, want run %q, %d files, and at least %d empty", raw, first.Prefix(), parts, parts-4)
	}
	if manifest.Files[0] != filepath.Base(shardname.Name(base, 0, parts)) {
		t.Errorf("first file %q, want %q", manifest.Files[0], shardname.Name(base, 0, parts))
	}
	waitSwept(t, dir)

	// The shards are the next run's input: how many words have each count.
	files, err := shardname.Expand(base + "@" + strconv.Itoa(parts))
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	src, err := eqmr.NewFileSource("counts", eqmr.WithFiles(files...), eqmr.WithFormat(eqmr.TSV))
	if err != nil {
		t.Fatalf("new file source: %v", err)
	}
	second, err := eqmr.New(eq, testPrefix(), eqmr.WithReduceShards(2), eqmr.WithSource(src))
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if err := second.SetupFrom(ctx); err != nil {
		t.Fatalf("second setup: %v", err)
	}
	byCount := func(ctx context.Context, _, count string, emit eqmr.EmitFunc) error {
		return emit(ctx, count, "1")
	}
	startRun(ctx, t, second, byCount, eqmr.SumReducer)
	if err := second.Wait(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	results, err := second.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if got, want := strings.Join(kvStrings(results), ","), "(1)=2,(2)=1,(3)=1"; got != want {
		t.Errorf("second run %q, want %q", got, want)
	}
}

// TestFileSinkRefusesAnotherRun writes a second run to output the first owns.
func TestFileSinkRefusesAnotherRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eq := newClient(ctx, t)
	base := filepath.Join(t.TempDir(), "out")
	sink, err := eqmr.NewFileSink("out", base, eqmr.TSV)
	if err != nil {
		t.Fatalf("new file sink: %v", err)
	}
	input := []*eqmr.KV{eqmr.NewKV("", "a b")}
	for i, want := range []string{"", "belongs to run"} {
		ctrl, err := eqmr.New(eq, testPrefix(), append(testOpts(1, 2), eqmr.WithSink(sink))...)
		if err != nil {
			t.Fatalf("new controller: %v", err)
		}
		err = ctrl.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{Mappers: 1, Reducers: 1})
		if want == "" && err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("run %d: got %v, want a failed run naming %q", i, err, want)
		}
	}
}

// stage writes kvs as one attempt at partition of partitions.
func stage(ctx context.Context, t *testing.T, sink eqmr.Sink, run string, partition, partitions int, kvs ...string) json.RawMessage {
	t.Helper()
	w, err := sink.Stage(ctx, run, partition, partitions)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	for _, kv := range kvs {
		k, v, _ := strings.Cut(kv, "=")
		if err := w.Write(ctx, k, v); err != nil {
			t.Fatalf("write %q: %v", kv, err)
		}
	}
	staged, err := w.Close(ctx)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	return staged
}

func TestFileSinkFirstCommitWins(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := filepath.Join(dir, "out")
	sink, err := eqmr.NewFileSink("out", base, eqmr.TSV)
	if err != nil {
		t.Fatalf("new file sink: %v", err)
	}
	a := stage(ctx, t, sink, "/run", 0, 1, "k=from a")
	b := stage(ctx, t, sink, "/run", 0, 1, "k=from b")
	for _, staged := range []json.RawMessage{b, a, b} {
		if err := sink.Commit(ctx, "/run", 0, staged); err != nil {
			t.Fatalf("commit %s: %v", staged, err)
		}
	}
	if err := sink.Finish(ctx, "/run", 1, nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := sink.Sweep(ctx, "/run"); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Committing again after the sweep finds the shard in place.
	if err := sink.Commit(ctx, "/run", 0, a); err != nil {
		t.Fatalf("commit after sweep: %v", err)
	}
	if got := readShards(ctx, t, base, 1, eqmr.TSV); !slices.Equal(got, []string{"k=from b"}) {
		t.Errorf("output %q, want the first commit's", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{".out.eqmr", "out-00000-of-00001", "out.manifest"}; !slices.Equal(names, want) {
		t.Errorf("output directory holds %q, want %q", names, want)
	}

	// Another run is refused permanently, and its sweep touches nothing.
	if _, err := sink.Stage(ctx, "/other", 0, 1); !isMove(err) {
		t.Errorf("stage for another run: got %v, want a MoveError", err)
	}
	if err := sink.Sweep(ctx, "/other"); err != nil {
		t.Errorf("sweep for another run: %v", err)
	}
}

func TestFileSinkSweepClaimsNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sink, err := eqmr.NewFileSink("out", filepath.Join(dir, "out"), eqmr.Lines)
	if err != nil {
		t.Fatalf("new file sink: %v", err)
	}
	// A run that failed before staging sweeps, then another run writes.
	if err := sink.Sweep(ctx, "/failed"); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("sweep of a run that never staged left %d entries", len(entries))
	}
	stage(ctx, t, sink, "/next", 0, 1, "k=v")
}

func TestFileSinkRefusesUnreadableLines(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		format     eqmr.FileFormat
		key, value string
		ok         bool
	}{
		{eqmr.TSV, "k", "a\tb", true},
		{eqmr.TSV, "k\t", "v", false},
		{eqmr.TSV, "k\n", "v", false},
		{eqmr.TSV, "k", "v\n", false},
		{eqmr.TSV, "k", "v\r", false},
		{eqmr.Lines, "k\t", "v\r inside", true},
		{eqmr.Lines, "k", "two\nlines", false},
	} {
		sink, err := eqmr.NewFileSink("out", filepath.Join(t.TempDir(), "out"), tc.format)
		if err != nil {
			t.Fatalf("new file sink: %v", err)
		}
		w, err := sink.Stage(ctx, "/run", 0, 1)
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		err = w.Write(ctx, tc.key, tc.value)
		if tc.ok && err != nil {
			t.Errorf("write %q=%q: %v", tc.key, tc.value, err)
		}
		if !tc.ok && !isMove(err) {
			t.Errorf("write %q=%q: got %v, want a MoveError", tc.key, tc.value, err)
		}
		if _, err := w.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	}
}

func TestNewFileSinkRejectsBadBase(t *testing.T) {
	for _, base := range []string{"", "out", "/", "/tmp/out/", "/tmp/../out"} {
		if _, err := eqmr.NewFileSink("out", base, eqmr.TSV); err == nil {
			t.Errorf("NewFileSink(%q) succeeded, want an error", base)
		}
	}
	if _, err := eqmr.NewFileSink("out", "/tmp/out", eqmr.FileFormat{}); err == nil {
		t.Error("NewFileSink with no format succeeded, want an error")
	}
}
