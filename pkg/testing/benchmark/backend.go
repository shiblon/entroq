package benchmark

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
)

const (
	seedBatchSize  = 250
	maxClaimMisses = 1_000
)

var backendPayload = json.RawMessage(`{"benchmark":true}`)

// BackendFactory opens an isolated client for one benchmark scenario. Setup is
// never timed, and each invocation must return a client with independent data.
type BackendFactory func(context.Context, *testing.B) (*entroq.EntroQ, error)

// OpenGRPC starts an in-process EntroQ gRPC service over opener and returns a
// client connected through bufconn. Both the client and service are closed by
// benchmark cleanup.
func OpenGRPC(ctx context.Context, b *testing.B, opener entroq.BackendOpener) (*entroq.EntroQ, error) {
	b.Helper()
	stop, dial, err := eqtest.StartService(ctx, opener)
	if err != nil {
		return nil, err
	}
	b.Cleanup(stop)
	return entroq.New(ctx, eqgrpc.Opener("bufnet",
		eqgrpc.WithNiladicDialer(dial),
		eqgrpc.WithInsecure()))
}

// RunBackendBenchmarks runs the common backend benchmark suite.
// Scenarios use only the public EntroQ API so every backend receives identical
// operations and setup data.
func RunBackendBenchmarks(b *testing.B, name string, open BackendFactory) {
	b.Helper()
	b.Run(name, func(b *testing.B) {
		b.Run("TryClaim/Empty", func(b *testing.B) {
			client, ctx := openBenchmarkClient(b, open)
			queue := benchmarkQueue(name, "empty")
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for range b.N {
				task, err := client.TryClaim(ctx, entroq.From(queue), entroq.ClaimFor(time.Minute))
				if err != nil {
					b.Fatal(err)
				}
				if task != nil {
					b.Fatalf("empty queue returned task %q", task.ID)
				}
			}
		})

		b.Run("ClaimComplete/Serial", func(b *testing.B) {
			client, ctx := openBenchmarkClient(b, open)
			queue := benchmarkQueue(name, "claim-serial")
			seedTasks(b, ctx, client, queue, 1)
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			var attempts uint64
			for range b.N {
				tries, err := claimAndComplete(ctx, client, queue)
				if err != nil {
					b.Fatal(err)
				}
				attempts += uint64(tries)
			}
			b.StopTimer()
			b.ReportMetric(float64(attempts)/float64(b.N), "tryclaims/op")
		})

		b.Run("ClaimComplete/Parallel", func(b *testing.B) {
			client, ctx := openBenchmarkClient(b, open)
			queue := benchmarkQueue(name, "claim-parallel")
			seedTasks(b, ctx, client, queue, max(64, 4*runtime.GOMAXPROCS(0)))
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			var attempts atomic.Uint64
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					tries, err := claimAndComplete(ctx, client, queue)
					if err != nil {
						b.Error(err)
						return
					}
					attempts.Add(uint64(tries))
				}
			})
			b.StopTimer()
			b.ReportMetric(float64(attempts.Load())/float64(b.N), "tryclaims/op")
		})

		for _, size := range []int{1, 32} {
			b.Run(fmt.Sprintf("Modify/Handoff%d", size), func(b *testing.B) {
				client, ctx := openBenchmarkClient(b, open)
				queue := benchmarkQueue(name, fmt.Sprintf("handoff-%d", size))
				tasks := seedTasks(b, ctx, client, queue, size)
				b.ReportAllocs()
				b.ResetTimer()
				b.StartTimer()
				for range b.N {
					args := make([]entroq.ModifyArg, 0, 2*size)
					for _, task := range tasks {
						args = append(args, task.Delete())
					}
					for range size {
						args = append(args, entroq.InsertingInto(queue, entroq.WithRawValue(backendPayload)))
					}
					resp, err := client.Modify(ctx, args...)
					if err != nil {
						b.Fatal(err)
					}
					if len(resp.InsertedTasks) != size {
						b.Fatalf("inserted %d tasks, want %d", len(resp.InsertedTasks), size)
					}
					tasks = resp.InsertedTasks
				}
			})
		}

		for _, size := range []int{1_000, 10_000} {
			b.Run(fmt.Sprintf("QueueStats/Depth%d", size), func(b *testing.B) {
				client, ctx := openBenchmarkClient(b, open)
				queue := benchmarkQueue(name, fmt.Sprintf("stats-%d", size))
				seedTasks(b, ctx, client, queue, size)
				query := entroq.MatchExact(queue)
				b.ReportAllocs()
				b.ResetTimer()
				b.StartTimer()
				for range b.N {
					stats, err := client.QueueStats(ctx, query)
					if err != nil {
						b.Fatal(err)
					}
					stat := stats[queue]
					if stat == nil {
						b.Fatalf("queue %q missing from stats", queue)
					}
					if got := stat.Size; got != size {
						b.Fatalf("queue size %d, want %d", got, size)
					}
				}
			})

			b.Run(fmt.Sprintf("Tasks/Depth%d", size), func(b *testing.B) {
				client, ctx := openBenchmarkClient(b, open)
				queue := benchmarkQueue(name, fmt.Sprintf("tasks-%d", size))
				seedTasks(b, ctx, client, queue, size)
				b.ReportAllocs()
				b.ResetTimer()
				b.StartTimer()
				for range b.N {
					tasks, err := client.Tasks(ctx, queue)
					if err != nil {
						b.Fatal(err)
					}
					if len(tasks) != size {
						b.Fatalf("listed %d tasks, want %d", len(tasks), size)
					}
				}
			})
		}
	})
}

func openBenchmarkClient(b *testing.B, open BackendFactory) (*entroq.EntroQ, context.Context) {
	b.Helper()
	b.StopTimer()
	ctx := b.Context()
	client, err := open(ctx, b)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := client.Close(); err != nil {
			b.Errorf("close benchmark client: %v", err)
		}
	})
	return client, ctx
}

func benchmarkQueue(backend, scenario string) string {
	return fmt.Sprintf("/benchmark/%s/%s/%s", backend, scenario, entroq.GenHex16())
}

func seedTasks(b *testing.B, ctx context.Context, client *entroq.EntroQ, queue string, count int) []*entroq.Task {
	b.Helper()
	tasks := make([]*entroq.Task, 0, count)
	for start := 0; start < count; start += seedBatchSize {
		n := min(seedBatchSize, count-start)
		args := make([]entroq.ModifyArg, n)
		for i := range args {
			args[i] = entroq.InsertingInto(queue, entroq.WithRawValue(backendPayload))
		}
		resp, err := client.Modify(ctx, args...)
		if err != nil {
			b.Fatalf("seed %q: %v", queue, err)
		}
		tasks = append(tasks, resp.InsertedTasks...)
	}
	return tasks
}

func claimAndComplete(ctx context.Context, client *entroq.EntroQ, queue string) (int, error) {
	for attempts := 1; ; attempts++ {
		task, err := client.TryClaim(ctx, entroq.From(queue), entroq.ClaimFor(time.Minute))
		if err != nil {
			return attempts, err
		}
		if task == nil {
			if attempts >= maxClaimMisses {
				return attempts, fmt.Errorf("queue %q remained empty after %d attempts", queue, attempts)
			}
			runtime.Gosched()
			continue
		}
		if _, err := client.Modify(ctx,
			task.Delete(),
			entroq.InsertingInto(queue, entroq.WithRawValue(backendPayload)),
		); err != nil {
			return attempts, err
		}
		return attempts, nil
	}
}
