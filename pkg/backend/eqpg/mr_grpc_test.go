package eqpg

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/examples/mrtest"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
)

// TestMapReduceOverGRPCPostgres runs the MapReduce workload over the gRPC
// transport against a real Postgres backend, under a deliberately short claim
// retry interval and repeated for load.
//
// This is the environment (gRPC + Postgres) where the lost-claim delivery race
// historically surfaced as ~30s stalls, and it also exercises the doc store
// heavily (the map phase tracks shard docs by a "shard/N" key range). Both the
// claim fix and the doc-key byte-order (COLLATE "C") fix are needed for it to
// complete: a stalled claim would hang the pipeline, and locale-collated doc
// key ranges would hide the shard docs and end the map phase prematurely.
func TestMapReduceOverGRPCPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gRPC+Postgres MapReduce load test in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	stop, dial, err := eqtest.StartService(ctx, Opener(pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(10),
		WithHeartbeat(5*time.Second),
	))
	if err != nil {
		t.Fatalf("start gRPC service over postgres: %v", err)
	}
	defer stop()

	client, err := entroq.New(ctx, eqgrpc.Opener("bufnet",
		eqgrpc.WithNiladicDialer(dial),
		eqgrpc.WithInsecure(),
		eqgrpc.WithClaimRetryInterval(50*time.Millisecond),
	))
	if err != nil {
		t.Fatalf("new gRPC client: %v", err)
	}
	defer client.Close()

	const (
		runs        = 5
		numDocs     = 15
		numMappers  = 8
		numReducers = 3
	)
	for i := 1; i <= runs; i++ {
		if !mrtest.MRCheck(ctx, client, numDocs, numMappers, numReducers) {
			t.Fatalf("MapReduce run %d/%d failed: a stalled claim or a mishandled doc key range breaks the pipeline (see logs)", i, runs)
		}
	}
}
