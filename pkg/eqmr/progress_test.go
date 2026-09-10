package eqmr_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/eqmr"
	"github.com/shiblon/entroq/pkg/eqmr/eqmrtest"
)

// TestProgressAccounting samples Progress throughout a live run and requires the
// counts to stay coherent: the three states always sum to the total, nothing
// ever moves backwards, and a finished run reports everything done.
func TestProgressAccounting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer eq.Close()

	const (
		mapShards    = 12
		reduceShards = 5
	)
	ctrl, err := eqmr.New(eq, eqmr.Config{
		Prefix:          "/progresstest/" + entroq.GenHex16(),
		MapShards:       mapShards,
		ReduceShards:    reduceShards,
		Lease:           5 * time.Second,
		ControlInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}

	bodies, _ := eqmrtest.Docs(400, 800, 60, 7)
	input := make([]*eqmr.KV, len(bodies))
	for i, b := range bodies {
		input[i] = eqmr.NewKV(nil, b)
	}

	sampleCtx, stopSampling := context.WithCancel(ctx)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		samples  int
		sawRun   bool
		maxDone  int
		problems []string
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-sampleCtx.Done():
				return
			case <-time.After(3 * time.Millisecond):
			}
			p, err := ctrl.Progress(ctx)
			if err != nil {
				continue // the run may not be set up yet
			}
			mu.Lock()
			samples++
			if p.Map.Total != mapShards {
				problems = append(problems, "map total drifted")
			}
			if p.Reduce.Total != reduceShards {
				problems = append(problems, "reduce total drifted")
			}
			if p.Map.Done+p.Map.Running+p.Map.Pending != p.Map.Total {
				problems = append(problems, "map states do not sum to total")
			}
			if p.Reduce.Done+p.Reduce.Running+p.Reduce.Pending != p.Reduce.Total {
				problems = append(problems, "reduce states do not sum to total")
			}
			if p.Map.Done < maxDone {
				problems = append(problems, "map done moved backwards")
			}
			maxDone = max(maxDone, p.Map.Done)
			if p.Map.Running > 0 || p.Reduce.Running > 0 {
				sawRun = true
			}
			if p.Quarantined != 0 {
				problems = append(problems, "unexpected quarantined task")
			}
			mu.Unlock()
		}
	}()

	if err := ctrl.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
		Mappers: 3, Reducers: 2,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	stopSampling()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for _, p := range problems {
		t.Error(p)
	}
	if samples < 5 {
		t.Errorf("only %d progress samples taken, want at least 5", samples)
	}
	if !sawRun {
		t.Error("never observed a unit in the running state")
	}

	final, err := ctrl.Progress(ctx)
	if err != nil {
		t.Fatalf("final progress: %v", err)
	}
	if final.Phase != eqmr.PhaseDone {
		t.Errorf("final phase %q, want %q", final.Phase, eqmr.PhaseDone)
	}
	if final.Map.Done != mapShards || final.Map.Running != 0 || final.Map.Pending != 0 {
		t.Errorf("final map progress %+v, want all %d done", final.Map, mapShards)
	}
	if final.Reduce.Done != reduceShards || final.Reduce.Running != 0 || final.Reduce.Pending != 0 {
		t.Errorf("final reduce progress %+v, want all %d done", final.Reduce, reduceShards)
	}
	t.Logf("%d samples; final map=%+v reduce=%+v", samples, final.Map, final.Reduce)
}

// TestProgressCountsEmptyPartitions pins that a partition nobody emitted to
// still reports as done. It writes an explicit empty result doc precisely so
// completion is a recorded fact rather than an absence.
func TestProgressCountsEmptyPartitions(t *testing.T) {
	ctx := context.Background()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer eq.Close()

	// Far more partitions than distinct keys, so most partitions get nothing.
	const reduceShards = 40
	ctrl, err := eqmr.New(eq, eqmr.Config{
		Prefix:          "/emptyparts/" + entroq.GenHex16(),
		MapShards:       2,
		ReduceShards:    reduceShards,
		Lease:           5 * time.Second,
		ControlInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}

	input := []*eqmr.KV{eqmr.NewKV(nil, []byte("alpha beta alpha"))}
	if err := ctrl.Run(ctx, input, eqmr.WordCountMapper, eqmr.SumReducer, eqmr.RunOptions{
		Mappers: 2, Reducers: 2,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	p, err := ctrl.Progress(ctx)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if p.Reduce.Done != reduceShards {
		t.Errorf("reduce done %d, want all %d partitions accounted for", p.Reduce.Done, reduceShards)
	}
	results, err := ctrl.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("got %d results, want 2 (alpha, beta)", len(results))
	}
}
