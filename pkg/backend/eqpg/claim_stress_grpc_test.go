package eqpg

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
	"golang.org/x/sync/errgroup"
)

// TestClaimContentionOverGRPCPostgres drives heavy concurrent claiming over the
// gRPC transport against a real Postgres backend, with a deliberately short
// claim retry interval. This is the environment (gRPC + Postgres) where the
// lost-claim delivery race historically showed up as ~30s stalls.
//
// Many workers claim-and-delete a fixed pool of tasks while re-issuing Claim
// aggressively. Every task must be consumed exactly the number of times it was
// produced: a claim that commits server-side but is discarded by the client
// would strand its task for the claim duration, so the consumers would fall
// short of the total within the deadline. With the fix the pool drains cleanly.
func TestClaimContentionOverGRPCPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gRPC+Postgres claim contention load test in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// A gRPC service backed by the shared Postgres instance.
	s, dial, err := eqtest.StartService(ctx, Opener(pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(10),
		WithHeartbeat(5*time.Second),
	))
	if err != nil {
		t.Fatalf("start gRPC service over postgres: %v", err)
	}
	defer s.Stop()

	newClient := func() (*entroq.EntroQ, error) {
		return entroq.New(ctx, eqgrpc.Opener("bufnet",
			eqgrpc.WithNiladicDialer(dial),
			eqgrpc.WithInsecure(),
		))
	}

	const (
		total   = 400
		workers = 10
		batch   = 100
	)
	queue := fmt.Sprintf("/stress/claims/%s", entroq.GenHex16())

	// Producer: insert the task pool in batches to keep setup quick.
	prod, err := newClient()
	if err != nil {
		t.Fatalf("producer client: %v", err)
	}
	defer prod.Close()
	for start := 0; start < total; start += batch {
		var args []entroq.ModifyArg
		for i := start; i < start+batch && i < total; i++ {
			args = append(args, entroq.InsertingInto(queue, entroq.WithValue(i)))
		}
		if _, err := prod.Modify(ctx, args...); err != nil {
			t.Fatalf("insert batch at %d: %v", start, err)
		}
	}

	// Consumers: distinct clients (distinct claimants) claim-and-delete until the
	// pool is drained, then release the rest.
	var consumed int64
	consumeCtx, consumeCancel := context.WithCancel(ctx)
	defer consumeCancel()

	g, gctx := errgroup.WithContext(consumeCtx)
	for range workers {
		g.Go(func() error {
			client, err := newClient()
			if err != nil {
				return err
			}
			defer client.Close()
			for {
				task, err := client.Claim(gctx, entroq.From(queue))
				if err != nil {
					if entroq.IsCanceled(err) {
						return nil
					}
					return fmt.Errorf("claim: %w", err)
				}
				if _, err := client.Modify(gctx, task.Delete()); err != nil {
					if entroq.IsCanceled(err) {
						return nil
					}
					return fmt.Errorf("delete: %w", err)
				}
				if atomic.AddInt64(&consumed, 1) >= total {
					consumeCancel() // pool drained; wake the other workers out of Claim
					return nil
				}
			}
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("consumers: %v", err)
	}

	if got := atomic.LoadInt64(&consumed); got != total {
		t.Fatalf("consumed %d tasks, want %d; a lost or stranded claim fell short", got, total)
	}
}
