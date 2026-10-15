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
)

// stateClaims counts claims of the run's state doc, and those that found it
// held.
type stateClaims struct {
	entroq.Backend
	claims, held atomic.Int64
}

func (b *stateClaims) ClaimDocs(ctx context.Context, cq *entroq.DocClaim) ([]*entroq.Doc, error) {
	docs, err := b.Backend.ClaimDocs(ctx, cq)
	if cq.Key == "state" {
		b.claims.Add(1)
		if _, ok := entroq.AsDependency(err); ok {
			b.held.Add(1)
		}
	}
	return docs, err
}

// fanInRun is a run with many splits and the workers to finish them fast, so
// their checks arrive in bursts.
type fanInRun struct {
	t         *testing.T
	eq        *entroq.EntroQ
	back      *stateClaims
	ctrl      *eqmr.Controller
	input     []*eqmr.KV
	histogram map[string]int

	runCtx context.Context
	wg     sync.WaitGroup
}

const fanInSplits = 200

func newFanInRun(ctx context.Context, t *testing.T) *fanInRun {
	t.Helper()
	r := &fanInRun{t: t}
	eq, err := entroq.New(ctx, func(ctx context.Context) (entroq.Backend, error) {
		back, err := eqmem.New(ctx)
		if err != nil {
			return nil, err
		}
		r.back = &stateClaims{Backend: back}
		return r.back, nil
	})
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	t.Cleanup(func() { eq.Close() })
	r.eq = eq

	r.ctrl, err = eqmr.New(eq, "/fanin/"+entroq.GenHex16(),
		eqmr.WithMapShards(fanInSplits),
		eqmr.WithReduceShards(8),
		eqmr.WithLease(2*time.Second),
	)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	bodies, histogram := eqmrtest.Docs(50, 50, fanInSplits, 17)
	r.histogram = histogram
	r.input = make([]*eqmr.KV, len(bodies))
	for i, b := range bodies {
		r.input[i] = eqmr.NewKV("", b)
	}
	if err := r.ctrl.Setup(ctx, r.input); err != nil {
		t.Fatalf("setup: %v", err)
	}

	runCtx, stop := context.WithCancel(ctx)
	r.runCtx = runCtx
	t.Cleanup(func() { stop(); r.wg.Wait() })
	return r
}

// start runs fn until the run's workers stop, or ctx ends.
func (r *fanInRun) start(ctx context.Context, fn func(context.Context) error) {
	r.wg.Add(1)
	go func() { defer r.wg.Done(); _ = fn(ctx) }()
}

// startWorkers starts the mappers and reducers.
func (r *fanInRun) startWorkers() {
	for range 16 {
		r.start(r.runCtx, func(ctx context.Context) error { return r.ctrl.RunMapper(ctx, eqmr.WordCountMapper) })
	}
	for range 4 {
		r.start(r.runCtx, func(ctx context.Context) error { return r.ctrl.RunReducer(ctx, eqmr.SumReducer) })
	}
}

// waitMapped waits until at least n splits are accepted, and fails if the map
// phase ended first.
func (r *fanInRun) waitMapped(ctx context.Context, n int) {
	r.t.Helper()
	for {
		p, err := r.ctrl.Progress(ctx)
		if err != nil {
			r.t.Fatalf("progress: %v", err)
		}
		if p.Phase != eqmr.PhaseMap {
			r.t.Fatalf("run reached %q before %d splits were seen accepted", p.Phase, n)
		}
		if p.Map.Done >= n {
			return
		}
		select {
		case <-ctx.Done():
			r.t.Fatalf("map phase made no progress: %v", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// finish waits for the run and checks its output.
func (r *fanInRun) finish(ctx context.Context) {
	r.t.Helper()
	if err := r.ctrl.Wait(ctx); err != nil {
		r.t.Fatalf("wait: %v", err)
	}
	requireCollected(ctx, r.t, r.eq, r.ctrl)
	results, err := r.ctrl.Results(ctx)
	if err != nil {
		r.t.Fatalf("results: %v", err)
	}
	if err := eqmrtest.VerifyHistogram(results, r.histogram); err != nil {
		r.t.Fatal(err)
	}
}

// TestControllerUnderFanIn finishes many splits at once. Each acceptance
// commits a check, but only the check that ends a phase claims the state doc,
// so the run claims it once per transition and never finds it held. Checks
// that arrive after their phase ends, here inserted once the run is done,
// retire without a claim.
func TestControllerUnderFanIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	r := newFanInRun(ctx, t)
	r.start(r.runCtx, r.ctrl.RunController)
	r.startWorkers()
	r.finish(ctx)

	var late []entroq.ModifyArg
	for range 10 {
		for _, q := range []string{r.ctrl.MapCheckQ(), r.ctrl.ReduceCheckQ()} {
			late = append(late, entroq.InsertingInto(q, entroq.WithValue(struct{}{})))
		}
	}
	if _, err := r.eq.Modify(ctx, late...); err != nil {
		t.Fatalf("insert late checks: %v", err)
	}
	if err := r.eq.WaitQueuesEmpty(ctx, entroq.MatchExact(r.ctrl.MapCheckQ(), r.ctrl.ReduceCheckQ())); err != nil {
		t.Fatalf("wait for late checks: %v", err)
	}

	// Map to reduce, and reduce to done.
	if got := r.back.claims.Load(); got != 2 {
		t.Errorf("state doc claimed %d times, want 2", got)
	}
	if got := r.back.held.Load(); got != 0 {
		t.Errorf("state doc found held %d times, want 0", got)
	}
}

// TestControllerRestart stops the controller partway through the map phase and
// starts another, which finishes the run from what is in EntroQ.
func TestControllerRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	r := newFanInRun(ctx, t)
	firstCtx, stopFirst := context.WithCancel(r.runCtx)
	firstDone := make(chan struct{})
	go func() { defer close(firstDone); _ = r.ctrl.RunController(firstCtx) }()
	r.startWorkers()

	r.waitMapped(ctx, fanInSplits/4)
	stopFirst()
	<-firstDone
	p, err := r.ctrl.Progress(ctx)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if p.Phase == eqmr.PhaseDone || p.Phase == eqmr.PhaseFailed {
		t.Fatalf("run reached %q before the controller stopped; nothing left to recover", p.Phase)
	}

	r.start(r.runCtx, r.ctrl.RunController)
	r.finish(ctx)
}

// TestSetupResumesRun sets up a run again partway through its map phase, and
// again once it is done, as a restarted launcher would. Neither brings back an
// accepted split, so the run finishes and leaves nothing behind.
func TestSetupResumesRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	r := newFanInRun(ctx, t)
	r.start(r.runCtx, r.ctrl.RunController)
	r.startWorkers()
	r.waitMapped(ctx, fanInSplits/4)
	if err := r.ctrl.Setup(ctx, r.input); err != nil {
		t.Fatalf("setup mid-run: %v", err)
	}
	r.finish(ctx)

	if err := r.ctrl.Setup(ctx, r.input); err != nil {
		t.Fatalf("setup after done: %v", err)
	}
	requireCollected(ctx, t, r.eq, r.ctrl)
}

// TestTwoControllers runs a second controller by mistake. The state doc's
// claim keeps the run correct; it may only cost time.
func TestTwoControllers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	r := newFanInRun(ctx, t)
	r.start(r.runCtx, r.ctrl.RunController)
	r.start(r.runCtx, r.ctrl.RunController)
	r.startWorkers()
	r.finish(ctx)
}
