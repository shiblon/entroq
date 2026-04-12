package eqmem

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/testing/benchmark"
)

// BatchPopulate fills the backend with n tasks spread evenly across multiple queues.
func BatchPopulate(ctx context.Context, c *entroq.EntroQ, n int, queues []string) error {
	const batchSize = 1000
	now := time.Now()
	for i := 0; i < n; i += batchSize {
		var ins []entroq.ModifyArg
		for j := i; j < i+batchSize && j < n; j++ {
			q := queues[(i+j)%len(queues)]
			ins = append(ins, entroq.InsertingInto(q, entroq.WithArrivalTime(now), entroq.WithValue("bench-value")))
		}
		if _, err := c.Modify(ctx, ins...); err != nil {
			return fmt.Errorf("populate batch: %w", err)
		}
	}
	return nil
}

func BenchmarkStatsContention_1M(b *testing.B) {
	ctx := context.Background()
	queues := []string{"/bench/q1", "/bench/q2", "/bench/q3", "/bench/prefix/a", "/bench/prefix/b"}

	client, err := entroq.New(ctx, Opener())
	if err != nil {
		b.Fatalf("failed to open client: %v", err)
	}
	defer client.Close()

	if err := BatchPopulate(ctx, client, 1000000, queues); err != nil {
		b.Fatalf("Populate failed: %v", err)
	}

	benchmark.RunContentionBenchmark(b, client, queues, 20)
}
