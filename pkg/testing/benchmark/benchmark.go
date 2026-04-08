package benchmark

import (
	"context"
	"log"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"golang.org/x/sync/errgroup"
)

// Distribution tracks a slice of latencies and reports percentiles.
type Distribution struct {
	mu        sync.Mutex
	latencies []time.Duration
}

// Add adds a latency sample to the distribution.
func (d *Distribution) Add(dur time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.latencies = append(d.latencies, dur)
}

// Report computes the stats and reports them to the benchmark.
func (d *Distribution) Report(b *testing.B) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.latencies) == 0 {
		return
	}
	sort.Slice(d.latencies, func(i, j int) bool {
		return d.latencies[i] < d.latencies[j]
	})

	n := len(d.latencies)
	p50 := d.latencies[n*50/100]
	p90 := d.latencies[n*90/100]
	p99 := d.latencies[n*99/100]
	p999 := d.latencies[n*999/1000]

	b.ReportMetric(float64(p50.Microseconds()), "p50-µs")
	b.ReportMetric(float64(p90.Microseconds()), "p90-µs")
	b.ReportMetric(float64(p99.Microseconds()), "p99-µs")
	b.ReportMetric(float64(p999.Microseconds()), "p99.9-µs")
}

// RunContentionBenchmark runs a heavy stress test on the provided backend.
// It assumes the backend has already been populated with enough tasks to satisfy the workers.
func RunContentionBenchmark(b *testing.B, client *entroq.EntroQ, queues []string, numWorkers int) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dist := &Distribution{}
	b.ResetTimer()

	g, gctx := errgroup.WithContext(ctx)

	// Background workers claiming/deleting across all queues
	for i := 0; i < numWorkers; i++ {
		g.Go(func() error {
			for {
				select {
				case <-gctx.Done():
					return nil
				default:
					t0 := time.Now()
					t, err := client.TryClaim(gctx, entroq.From(queues...), entroq.ClaimFor(time.Second))
					if err != nil {
						if entroq.IsCanceled(err) {
							return nil
						}
						continue
					}
					if t != nil {
						dist.Add(time.Since(t0))
						client.Modify(gctx, entroq.Deleting(t.ID, t.Version))
					}
				}
			}
		})
	}

	// Hammer QueueStats in background to ensure that reading doesn't degrade normal operation.
	g.Go(func() error {
		for {
			select {
			case <-gctx.Done():
				return nil
			default:
				client.QueueStats(gctx, entroq.MatchPrefix("/bench/prefix"))
				client.QueueStats(gctx, entroq.MatchExact("/bench/q1"))
			}
		}
	})

	// Benchmark subject: performance of a specific worker under load.
	for i := 0; i < b.N; i++ {
		t0 := time.Now()
		t, err := client.TryClaim(ctx, entroq.From(queues[0]), entroq.ClaimFor(time.Second))
		if err == nil && t != nil {
			dist.Add(time.Since(t0))
			client.Modify(ctx, entroq.Deleting(t.ID, t.Version))
		}
	}

	cancel()
	if err := g.Wait(); err != nil {
		log.Fatalf("Error waiting on benchmark routines: %v", err)
	}
	dist.Report(b)
}
