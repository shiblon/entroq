package eqmr_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiblon/entroq/pkg/eqmr"
	"github.com/shiblon/entroq/pkg/shardname"
)

// crew runs workers that restart when killed, each incarnation under its own
// context, as a supervisor restarts a crashed process.
type crew struct {
	ctx   context.Context
	wg    sync.WaitGroup
	mu    sync.Mutex
	kills []func()
	count atomic.Int64
}

func newCrew(ctx context.Context) *crew { return &crew{ctx: ctx} }

// start runs fn, restarting it whenever it returns, until the crew's context
// ends. kill ends the current incarnation.
func (c *crew) start(fn func(context.Context) error) {
	c.mu.Lock()
	i := len(c.kills)
	c.kills = append(c.kills, func() {})
	c.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for c.ctx.Err() == nil {
			ctx, cancel := context.WithCancel(c.ctx)
			c.mu.Lock()
			c.kills[i] = cancel
			c.mu.Unlock()
			_ = fn(ctx)
			cancel()
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

// killOne ends a random worker's current incarnation.
func (c *crew) killOne(rng *rand.Rand) {
	c.mu.Lock()
	kill := c.kills[rng.IntN(len(c.kills))]
	c.mu.Unlock()
	kill()
	c.count.Add(1)
}

// verifyManifest checks base.manifest against the files it lists, and returns
// the number of records it counts.
func verifyManifest(t *testing.T, base string, parts int) int {
	t.Helper()
	raw, err := os.ReadFile(base + ".manifest")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Files []struct {
			Name    string `json:"name"`
			SHA256  string `json:"sha256"`
			Records int    `json:"records"`
			Bytes   int    `json:"bytes"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(manifest.Files) != parts {
		t.Fatalf("manifest lists %d files, want %d", len(manifest.Files), parts)
	}
	records := 0
	for i, f := range manifest.Files {
		if want := filepath.Base(shardname.Name(base, i, parts)); f.Name != want {
			t.Errorf("file %d is %q, want %q", i, f.Name, want)
		}
		body, err := os.ReadFile(filepath.Join(filepath.Dir(base), f.Name))
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		sum := sha256.Sum256(body)
		lines := strings.Count(string(body), "\n")
		if f.SHA256 != hex.EncodeToString(sum[:]) || f.Bytes != len(body) || f.Records != lines {
			t.Errorf("manifest says %s has sha256 %s, %d bytes, %d records; it has %x, %d, %d",
				f.Name, f.SHA256, f.Bytes, f.Records, sum, len(body), lines)
		}
		records += f.Records
	}
	return records
}

// TestFileRunUnderFaults runs files through a whole run, file store included,
// while workers and the controller are killed at random, every task is
// duplicated, and a second launcher sets the run up again mid-map. The output
// must be exact, its manifest true, and nothing left behind.
func TestFileRunUnderFaults(t *testing.T) {
	for seed := range uint64(3) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			soakFiles(t, seed)
		})
	}
}

func soakFiles(t *testing.T, seed uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	eq := newClient(ctx, t)
	rng := rand.New(rand.NewPCG(seed, 0x50a4))
	root := t.TempDir()

	const (
		splits = 10
		parts  = 5
	)
	input, histogram := wordCountFixture(t, 40, 3000, 30)
	files, err := shardname.Expand(filepath.Join(root, "in", "words") + fmt.Sprintf("@%d", splits))
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	contents := make(map[string]string)
	for i, kv := range input {
		rel, _ := filepath.Rel(root, files[i%splits])
		contents[rel] += kv.Value + "\n"
	}
	writeFiles(t, root, contents)

	store, err := eqmr.NewFileStore("scratch", filepath.Join(root, "scratch"))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	base := filepath.Join(root, "out", "counts")
	sink, err := eqmr.NewFileSink("counts", base, eqmr.TSV)
	if err != nil {
		t.Fatalf("new file sink: %v", err)
	}
	src, err := eqmr.NewFileSource("words", eqmr.WithFiles(files...))
	if err != nil {
		t.Fatalf("new file source: %v", err)
	}
	prefix := testPrefix()
	opts := []eqmr.Option{
		eqmr.WithReduceShards(parts),
		eqmr.WithIntermediateStore(store),
		eqmr.WithSink(sink),
		eqmr.WithSource(src),
		eqmr.WithLease(500 * time.Millisecond),
		eqmr.WithMaxClaims(-1),
	}
	ctrl, err := eqmr.New(eq, prefix, opts...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if err := ctrl.SetupFrom(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	duplicateTasks(ctx, t, eq, ctrl.MapQ())

	// Slow enough that kills land mid-task.
	mapFn := func(ctx context.Context, key, value string, emit eqmr.EmitFunc) error {
		time.Sleep(10 * time.Millisecond)
		return eqmr.WordCountMapper(ctx, key, value, emit)
	}
	reduceFn := func(ctx context.Context, in eqmr.ReducerInput) (string, error) {
		time.Sleep(3 * time.Millisecond)
		return eqmr.SumReducer(ctx, in)
	}

	started := time.Now()
	runCtx, stop := context.WithCancel(ctx)
	c := newCrew(runCtx)
	defer func() { stop(); c.wg.Wait() }()
	c.start(ctrl.RunController)
	for range 4 {
		c.start(func(ctx context.Context) error { return ctrl.RunMapper(ctx, mapFn) })
	}
	for range 3 {
		c.start(func(ctx context.Context) error { return ctrl.RunReducer(ctx, reduceFn) })
	}

	killerDone := make(chan struct{})
	go func() {
		defer close(killerDone)
		for {
			select {
			case <-runCtx.Done():
				return
			case <-time.After(time.Duration(20+rng.IntN(60)) * time.Millisecond):
				c.killOne(rng)
			}
		}
	}()

	waitMappedFiles(ctx, t, ctrl, splits/3)
	relauncher, err := eqmr.New(eq, prefix, opts...)
	if err != nil {
		t.Fatalf("new relauncher: %v", err)
	}
	if err := relauncher.SetupFrom(ctx); err != nil {
		t.Fatalf("setup mid-run: %v", err)
	}
	waitForPhase(ctx, t, ctrl, eqmr.PhaseReduce)
	duplicateTasks(ctx, t, eq, ctrl.ReduceQ())

	if err := ctrl.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	// A clean run takes about a second; a task held back by a retry delay
	// takes thirty.
	if took := time.Since(started); took > 20*time.Second {
		t.Errorf("run took %v under faults; a task waited out a retry delay", took)
	}
	// Collection and sweeps need the controller; stop killing it first.
	stop()
	<-killerDone
	c.wg.Wait()
	finish := newCrew(ctx)
	defer func() { cancel(); finish.wg.Wait() }()
	finish.start(ctrl.RunController)

	got := readShards(ctx, t, base, parts, eqmr.TSV)
	slices.Sort(got)
	var want []string
	for word, n := range histogram {
		want = append(want, fmt.Sprintf("%s=%d", word, n))
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("output has %d records, want %d; first difference near %v", len(got), len(want), firstDiff(got, want))
	}
	if n := verifyManifest(t, base, parts); n != len(want) {
		t.Errorf("manifest counts %d records, want %d", n, len(want))
	}
	requireCollected(ctx, t, eq, ctrl)
	waitSwept(t, filepath.Dir(base))
	deadline := time.Now().Add(10 * time.Second)
	for len(runDirs(t, filepath.Join(root, "scratch"))) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("file store kept %v", runDirs(t, filepath.Join(root, "scratch")))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("seed %d: %d kills", seed, c.count.Load())
	if c.count.Load() == 0 {
		t.Error("no worker was killed; the soak injected no faults")
	}
}

// waitMappedFiles waits until n splits are accepted.
func waitMappedFiles(ctx context.Context, t *testing.T, ctrl *eqmr.Controller, n int) {
	t.Helper()
	for {
		p, err := ctrl.Progress(ctx)
		if err != nil {
			t.Fatalf("progress: %v", err)
		}
		if p.Map.Done >= n || p.Phase != eqmr.PhaseMap {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("map phase made no progress: %v", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func firstDiff(got, want []string) string {
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			return fmt.Sprintf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
	return fmt.Sprintf("index %d", min(len(got), len(want)))
}
