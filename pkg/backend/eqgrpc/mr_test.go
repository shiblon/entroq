package eqgrpc_test

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/eqmr/eqmrtest"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
)

// TestGRPCMapReduce runs the MapReduce contract over the gRPC transport.
func TestGRPCMapReduce(t *testing.T) {
	RunQTest(t, eqtest.MapReduce)
}

// TestMapReduceOverGRPCMem runs the MapReduce workload over gRPC repeatedly,
// for load. A stalled claim delivery hangs the pipeline, and the map phase
// exercises the doc store heavily through a "split/N" key range.
//
// It needs no Docker, so the gRPC transport keeps this coverage when the
// equivalent Postgres load test cannot run. What it does not cover is
// everything specific to Postgres, the doc key byte-order collation above all.
func TestMapReduceOverGRPCMem(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	stop, dial, err := eqtest.StartService(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("start gRPC service over eqmem: %v", err)
	}
	defer stop()

	client, err := entroq.New(ctx, eqgrpc.Opener("bufnet",
		eqgrpc.WithNiladicDialer(dial),
		eqgrpc.WithInsecure(),
	))
	if err != nil {
		t.Fatalf("new gRPC client: %v", err)
	}
	defer client.Close()

	const (
		// Many more runs than the Postgres load test affords, because the point
		// is interleavings and this backend is nearly free: the whole loop is a
		// few seconds.
		runs        = 50
		numDocs     = 15
		numMappers  = 8
		numReducers = 3
	)
	for i := 1; i <= runs; i++ {
		if err := eqmrtest.QuickCheck(ctx, client, numDocs, numMappers, numReducers); err != nil {
			t.Fatalf("MapReduce run %d/%d failed (a stalled claim hangs the pipeline): %v", i, runs, err)
		}
	}
}
