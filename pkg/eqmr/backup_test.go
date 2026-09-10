package eqmr_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/eqmr"
	"github.com/shiblon/entroq/pkg/eqmr/eqmrtest"
	"github.com/shiblon/entroq/pkg/worker"
)

// TestBackupTasksRaceWithoutCorrupting is the backup-worker (speculative
// execution) guarantee: a duplicate task for a unit already in progress may run
// concurrently, and whichever commits first wins while the loser's work is
// discarded in full.
//
// Every map task is duplicated, and the mapper is slow enough that both copies
// of a split are genuinely in flight at once. Two things must hold. Mappers
// must actually run more times than there are splits, proving duplicates race
// rather than block on a claim; and the word counts must be exactly right,
// proving no split's output was recorded twice.
func TestBackupTasksRaceWithoutCorrupting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer eq.Close()

	const mapShards = 4
	ctrl, err := eqmr.New(eq, "/backuptest/"+entroq.GenHex16(),
		eqmr.WithMapShards(mapShards),
		eqmr.WithReduceShards(3),
		eqmr.WithLease(10*time.Second),
		eqmr.WithControlInterval(20*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}

	bodies, histogram := eqmrtest.Docs(40, 200, 8, 11)
	input := make([]*eqmr.KV, len(bodies))
	for i, b := range bodies {
		input[i] = eqmr.NewKV("", b)
	}
	if err := ctrl.Setup(ctx, input); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Duplicate every map task, exactly as a straggler policy would for one.
	tasks, err := eq.Tasks(ctx, ctrl.MapQ())
	if err != nil {
		t.Fatalf("list map tasks: %v", err)
	}
	if len(tasks) != mapShards {
		t.Fatalf("expected %d map tasks, got %d", mapShards, len(tasks))
	}
	var dupes []entroq.ModifyArg
	for _, task := range tasks {
		dupes = append(dupes, entroq.InsertingInto(ctrl.MapQ(), entroq.WithRawValue(task.Value)))
	}
	if _, err := eq.Modify(ctx, dupes...); err != nil {
		t.Fatalf("duplicate map tasks: %v", err)
	}

	// A slow mapper, so both copies of a split overlap instead of one finishing
	// before the other starts.
	var invocations atomic.Int64
	slowMapper := func(ctx context.Context, key, value string, emit eqmr.EmitFunc) error {
		invocations.Add(1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
		return eqmr.WordCountMapper(ctx, key, value, emit)
	}

	runCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	var wg sync.WaitGroup
	start := func(run func(context.Context) error) {
		wg.Add(1)
		go func() { defer wg.Done(); _ = run(runCtx) }()
	}
	// One mapper per task, so every duplicate is claimed simultaneously.
	for range mapShards * 2 {
		start(func(ctx context.Context) error {
			return ctrl.MapperWorker(slowMapper).Run(ctx,
				worker.Watching(ctrl.MapQ()), worker.WithLease(10*time.Second))
		})
	}
	for range 2 {
		start(func(ctx context.Context) error {
			return ctrl.ReducerWorker(eqmr.SumReducer).Run(ctx,
				worker.Watching(ctrl.ReduceQ()), worker.WithLease(10*time.Second))
		})
	}
	start(func(ctx context.Context) error {
		return ctrl.ControlWorker().Run(ctx,
			worker.Watching(ctrl.ControlQ()), worker.WithLease(10*time.Second))
	})

	if err := ctrl.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	stopWorkers()
	wg.Wait()

	// The mapper function runs once per input RECORD, not once per task, so a
	// run with no duplicates calls it exactly len(bodies) times. Exceeding that
	// is the proof that duplicate tasks executed concurrently instead of
	// blocking on a claim held by the original.
	baseline := int64(len(bodies))
	if got := invocations.Load(); got <= baseline {
		t.Errorf("mapper ran %d times, baseline for a duplicate-free run is %d; duplicates were blocked rather than racing", got, baseline)
	} else {
		t.Logf("mapper ran %d times against a duplicate-free baseline of %d: duplicates raced", got, baseline)
	}

	results, err := ctrl.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if err := eqmrtest.VerifyHistogram(results, histogram); err != nil {
		t.Fatalf("duplicate tasks corrupted the output: %v", err)
	}

	p, err := ctrl.Progress(ctx)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if p.Quarantined != 0 {
		t.Errorf("%d task(s) quarantined; a worker that merely lost a race must not be", p.Quarantined)
	}
	t.Logf("final: map=%+v reduce=%+v", p.Map, p.Reduce)
}
