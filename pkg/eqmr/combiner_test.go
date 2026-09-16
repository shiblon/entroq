package eqmr_test

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/eqmr"
	"github.com/shiblon/entroq/pkg/worker"
)

// mapOutBytes runs only the map phase and reports the total size of its durable
// run payloads: the intermediate data the reduce phase has to move.
func mapOutBytes(t *testing.T, input []*eqmr.KV, combiner eqmr.Combiner, mapShards int) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer eq.Close()

	ctrl, err := eqmr.New(eq, testPrefix(), testOpts(mapShards, 4)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if err := ctrl.Setup(ctx, input); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var opts []eqmr.MapperOption
	if combiner != nil {
		opts = append(opts, eqmr.WithCombiner(combiner))
	}
	mapCtx, stopMappers := context.WithCancel(ctx)
	defer stopMappers()
	for range 4 {
		go func() {
			_ = ctrl.MapperWorker(eqmr.WordCountMapper, opts...).Run(mapCtx,
				worker.Watching(ctrl.MapQ()), worker.WithLease(5*time.Second))
		}()
	}

	// The map phase barrier: split docs are deleted in the same Modify that
	// writes the map outputs, so their absence means every map output is durable.
	for {
		splits, err := eq.Docs(ctx, &entroq.DocQuery{
			Namespace: ctrl.DocNS(), KeyStart: "split/", KeyEnd: "split0",
			OmitValues: true, Limit: 1,
		})
		if err != nil {
			t.Fatalf("check splits: %v", err)
		}
		if len(splits) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for the map phase")
		case <-time.After(10 * time.Millisecond):
		}
	}
	stopMappers()

	outs, err := eq.Docs(ctx, &entroq.DocQuery{
		Namespace: ctrl.DocNS(), KeyStart: "run/", KeyEnd: "run0",
	})
	if err != nil {
		t.Fatalf("read map outputs: %v", err)
	}
	total := 0
	for _, d := range outs {
		total += len(d.Content)
	}
	return total
}

// TestCombinerShrinksMapOutput pins that a Combiner actually does something.
// TestCombinerDoesNotChangeResults proves it is safe, but a Combiner that
// returned its input untouched would pass that test too; this one fails if the
// combine step stops being applied.
//
// The saving is bounded by how many values a single mapper accumulates for one
// key, which is the number of input records per split. Both effects are
// asserted: that combining shrinks the map output, and that the saving grows as
// splits get coarser.
func TestCombinerShrinksMapOutput(t *testing.T) {
	input, _ := wordCountFixture(t, 50, 40000, 200)

	coarsePlain := mapOutBytes(t, input, nil, 2)
	coarseComb := mapOutBytes(t, input, eqmr.SumCombiner, 2)
	finePlain := mapOutBytes(t, input, nil, 25)
	fineComb := mapOutBytes(t, input, eqmr.SumCombiner, 25)

	t.Logf("2 splits  (100 recs/split): plain %d B, combined %d B (%.1fx)",
		coarsePlain, coarseComb, float64(coarsePlain)/float64(coarseComb))
	t.Logf("25 splits (  8 recs/split): plain %d B, combined %d B (%.1fx)",
		finePlain, fineComb, float64(finePlain)/float64(fineComb))

	if coarseComb >= coarsePlain/2 {
		t.Errorf("combiner barely helped at 2 splits: %d B vs %d B plain, want less than half",
			coarseComb, coarsePlain)
	}
	if fineComb >= finePlain {
		t.Errorf("combiner did not shrink the map output at 25 splits: %d B vs %d B plain",
			fineComb, finePlain)
	}
	coarseRatio := float64(coarsePlain) / float64(coarseComb)
	fineRatio := float64(finePlain) / float64(fineComb)
	if coarseRatio <= fineRatio {
		t.Errorf("coarser splits should combine better: %.1fx at 2 splits vs %.1fx at 25",
			coarseRatio, fineRatio)
	}
}
