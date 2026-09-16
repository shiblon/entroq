package eqmr_test

import (
	"context"
	"strings"
	"testing"

	"github.com/shiblon/entroq/pkg/eqmr"
)

func TestResultsRequireCompletedRun(t *testing.T) {
	ctx := context.Background()
	eq := newClient(ctx, t)

	ctrl, err := eqmr.New(eq, testPrefix(), testOpts(1, 1)...)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if err := ctrl.Setup(ctx, []*eqmr.KV{eqmr.NewKV("", "still mapping")}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if results, err := ctrl.Results(ctx); err == nil {
		t.Fatalf("Results returned %v before the run completed", results)
	} else if !strings.Contains(err.Error(), `run is in "map" phase, not "done"`) {
		t.Fatalf("Results error %q does not describe the incomplete run", err)
	}

	if results, err := ctrl.ResultsForPartition(ctx, 0); err == nil {
		t.Fatalf("ResultsForPartition returned %v before the run completed", results)
	} else if !strings.Contains(err.Error(), `run is in "map" phase, not "done"`) {
		t.Fatalf("ResultsForPartition error %q does not describe the incomplete run", err)
	}
}
