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
	if n := duplicateTasks(ctx, t, eq, ctrl.MapQ()); n != mapShards {
		t.Fatalf("expected %d map tasks, got %d", mapShards, n)
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
	start(ctrl.RunController)

	if err := ctrl.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	// Every run, the losers' included, is collected once the controller has
	// judged its finish task. Nothing but results may remain.
	requireCollected(ctx, t, eq, ctrl)
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

// TestDuplicateReduceTasksRace is the reduce-side backup guarantee. Every reduce
// task is duplicated before any reducer starts, so each partition is reduced
// twice and two finish tasks reach the controller. Exactly one result per
// partition must be recorded, the loser must not be quarantined, and every
// run, the loser's consumed inputs included, must still be collected.
func TestDuplicateReduceTasksRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer eq.Close()

	ctrl, err := eqmr.New(eq, "/reducebackup/"+entroq.GenHex16(),
		eqmr.WithMapShards(4),
		eqmr.WithReduceShards(3),
		eqmr.WithLease(10*time.Second),
	)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	bodies, histogram := eqmrtest.Docs(40, 200, 8, 13)
	input := make([]*eqmr.KV, len(bodies))
	for i, b := range bodies {
		input[i] = eqmr.NewKV("", b)
	}
	if err := ctrl.Setup(ctx, input); err != nil {
		t.Fatalf("setup: %v", err)
	}

	runCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	var wg sync.WaitGroup
	start := func(run func(context.Context) error) {
		wg.Add(1)
		go func() { defer wg.Done(); _ = run(runCtx) }()
	}
	start(ctrl.RunController)
	start(func(ctx context.Context) error { return ctrl.RunMapper(ctx, eqmr.WordCountMapper) })

	// Wait for the reduce fan-out, then duplicate every reduce task.
	waitForPhase(ctx, t, ctrl, eqmr.PhaseReduce)
	partitions := duplicateTasks(ctx, t, eq, ctrl.ReduceQ())

	// One reducer per task, so both copies of a partition are claimed at once.
	var reductions atomic.Int64
	countingReducer := func(ctx context.Context, in eqmr.ReducerInput) (string, error) {
		reductions.Add(1)
		return eqmr.SumReducer(ctx, in)
	}
	for range 2 * partitions {
		start(func(ctx context.Context) error { return ctrl.RunReducer(ctx, countingReducer) })
	}

	if err := ctrl.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	requireCollected(ctx, t, eq, ctrl)
	stopWorkers()
	wg.Wait()

	if got, distinct := reductions.Load(), int64(len(histogram)); got <= distinct {
		t.Logf("reducer ran %d times for %d keys: duplicates did not overlap this time", got, distinct)
	}
	results, err := ctrl.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if err := eqmrtest.VerifyHistogram(results, histogram); err != nil {
		t.Fatalf("duplicate reduce tasks corrupted the output: %v", err)
	}
	p, err := ctrl.Progress(ctx)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if p.Quarantined != 0 {
		t.Errorf("%d task(s) quarantined; a reducer that merely lost a race must not be", p.Quarantined)
	}
}

// TestControllerReplicasUnderFanIn finishes many splits at once against several
// controller replicas, so finish handlers constantly lose races for the state
// doc. A lost race is re-decided in place rather than requeued, so heavy
// contention must cost milliseconds, not readiness passes.
func TestControllerReplicasUnderFanIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer eq.Close()

	const splits = 200
	ctrl, err := eqmr.New(eq, "/fanin/"+entroq.GenHex16(),
		eqmr.WithMapShards(splits),
		eqmr.WithReduceShards(8),
	)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	bodies, histogram := eqmrtest.Docs(50, 50, splits, 17)
	input := make([]*eqmr.KV, len(bodies))
	for i, b := range bodies {
		input[i] = eqmr.NewKV("", b)
	}
	if err := ctrl.Setup(ctx, input); err != nil {
		t.Fatalf("setup: %v", err)
	}

	runCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	var wg sync.WaitGroup
	start := func(run func(context.Context) error) {
		wg.Add(1)
		go func() { defer wg.Done(); _ = run(runCtx) }()
	}
	for range 3 {
		start(ctrl.RunController)
	}
	for range 16 {
		start(func(ctx context.Context) error { return ctrl.RunMapper(ctx, eqmr.WordCountMapper) })
	}
	for range 4 {
		start(func(ctx context.Context) error { return ctrl.RunReducer(ctx, eqmr.SumReducer) })
	}

	began := time.Now()
	if err := ctrl.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	elapsed := time.Since(began)
	requireCollected(ctx, t, eq, ctrl)
	stopWorkers()
	wg.Wait()

	// Even one finish per readiness pass (5s) would take minutes here.
	if elapsed > 10*time.Second {
		t.Errorf("run took %v; lost state races are waiting on the queue", elapsed)
	}
	results, err := ctrl.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if err := eqmrtest.VerifyHistogram(results, histogram); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d splits through 3 controller replicas in %v", splits, elapsed)
}
