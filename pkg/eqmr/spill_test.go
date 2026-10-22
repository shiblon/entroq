package eqmr_test

import (
	"context"
	"testing"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/eqmr"
	"github.com/shiblon/entroq/pkg/eqmr/eqmrtest"
)

func TestSpilledIntermediateRunsPreserveResults(t *testing.T) {
	ctx := context.Background()
	input, histogram := wordCountFixture(t, 100, 5000, 10)

	for _, combiner := range []eqmr.Combiner{nil, eqmr.SumCombiner} {
		name := "plain"
		if combiner != nil {
			name = "combined"
		}
		t.Run(name, func(t *testing.T) {
			eq := newClient(ctx, t)
			ctrl, err := eqmr.New(eq, testPrefix(), testOpts(2, 3)...)
			if err != nil {
				t.Fatalf("new controller: %v", err)
			}
			if err := ctrl.Setup(ctx, input); err != nil {
				t.Fatalf("setup: %v", err)
			}
			opts := []eqmr.MapperOption{eqmr.WithIntermediateRunBytes(512)}
			if combiner != nil {
				opts = append(opts, eqmr.WithCombiner(combiner))
			}
			mapOnly(ctx, t, eq, ctrl, opts...)

			runs, err := eq.Docs(ctx, &entroq.DocQuery{
				Namespace:  ctrl.DocNS(),
				KeyStart:   "run/",
				KeyEnd:     "run0",
				OmitValues: true,
			})
			if err != nil {
				t.Fatalf("list intermediate runs: %v", err)
			}
			if len(runs) <= ctrl.Config().MapShards {
				t.Errorf("got %d durable run chunks, want more than %d to prove spilling occurred", len(runs), ctrl.Config().MapShards)
			}

			runCtx, stop := context.WithCancel(ctx)
			defer stop()
			go func() { _ = ctrl.RunController(runCtx) }()
			for range 2 {
				go func() { _ = ctrl.RunReducer(runCtx, eqmr.SumReducer) }()
			}
			if err := ctrl.Wait(runCtx); err != nil {
				t.Fatalf("wait: %v", err)
			}
			results, err := ctrl.Results(ctx)
			if err != nil {
				t.Fatalf("results: %v", err)
			}
			if err := eqmrtest.VerifyHistogram(results, histogram); err != nil {
				t.Fatal(err)
			}
			requireCollected(ctx, t, eq, ctrl)
		})
	}
}
