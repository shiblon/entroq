package benchmark

import (
	"context"
	"fmt"
	"io"
	"log"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/examples/mr"
)

const (
	mapReduceDocuments   = 1_000
	mapReduceMappers     = 16
	mapReduceReducers    = 4
	mapReduceWordKinds   = 10
	mapReduceWordRepeats = 100
)

type statsMode struct {
	name     string
	interval time.Duration
}

type queueHighWater struct {
	queued       int
	claimed      int
	mapInput     int
	mapResult    int
	reduceInput  int
	reduceResult int
}

type statsSamples struct {
	latencies []time.Duration
	highWater queueHighWater
}

// RunMapReduceLoadBenchmarks measures a deterministic MapReduce workload through
// a backend client, both alone and while QueueStats polls the workload queues.
// Run it with a fixed iteration count so every mode processes the same number
// of documents, for example -benchtime=3x.
func RunMapReduceLoadBenchmarks(b *testing.B, name string, open BackendFactory) {
	b.Helper()
	previousLogOutput := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(previousLogOutput)

	modes := []statsMode{
		{name: "Baseline"},
		{name: "Stats250ms", interval: 250 * time.Millisecond},
		{name: "Stats5s", interval: 5 * time.Second},
	}

	b.Run(name, func(b *testing.B) {
		for _, mode := range modes {
			b.Run(mode.name, func(b *testing.B) {
				client, ctx := openBenchmarkClient(b, open)
				input := mapReduceInput()
				root := fmt.Sprintf("/mrb/%s/%s/%s", name, strings.ToLower(mode.name), entroq.GenHex16())

				b.ReportAllocs()
				b.ResetTimer()
				b.StartTimer()
				var (
					cancelSamples context.CancelFunc
					samplesDone   chan statsSampleResult
				)
				if mode.interval > 0 {
					var sampleCtx context.Context
					sampleCtx, cancelSamples = context.WithCancel(ctx)
					b.Cleanup(cancelSamples)
					samplesDone = make(chan statsSampleResult, 1)
					go func() {
						samples, err := sampleQueueStats(sampleCtx, client, root, mode.interval)
						samplesDone <- statsSampleResult{samples: samples, err: err}
					}()
				}

				var workloadDuration time.Duration
				prefixes := make([]string, 0, b.N)
				for range b.N {
					prefix := root + "/" + entroq.GenHex16()
					if len(prefix) > 64 {
						b.Fatalf("MapReduce namespace %q is %d bytes, want at most 64", prefix, len(prefix))
					}
					start := time.Now()
					err := mr.RunAll(ctx, client, prefix, input, mr.WordCountMapper, mr.SumReducer,
						mapReduceMappers, mapReduceReducers)
					workloadDuration += time.Since(start)
					if err != nil {
						b.Fatalf("MapReduce: %v", err)
					}
					prefixes = append(prefixes, prefix)
				}
				b.StopTimer()

				if cancelSamples != nil {
					cancelSamples()
					result := <-samplesDone
					if result.err != nil {
						b.Fatalf("sample queue stats: %v", result.err)
					}
					result.samples.report(b, workloadDuration, b.N)
				}
				for _, prefix := range prefixes {
					verifyMapReduceResults(b, ctx, client, prefix)
				}
				b.ReportMetric(float64(mapReduceDocuments*b.N)/workloadDuration.Seconds(), "docs/s")
			})
		}
	})
}

type statsSampleResult struct {
	samples statsSamples
	err     error
}

func mapReduceInput() []*mr.KV {
	words := make([]string, 0, mapReduceWordKinds*mapReduceWordRepeats)
	for range mapReduceWordRepeats {
		for word := range mapReduceWordKinds {
			words = append(words, strconv.Itoa(word))
		}
	}
	payload := []byte(strings.Join(words, " "))
	input := make([]*mr.KV, mapReduceDocuments)
	for i := range input {
		input[i] = mr.NewKV(nil, payload)
	}
	return input
}

func verifyMapReduceResults(b *testing.B, ctx context.Context, client *entroq.EntroQ, prefix string) {
	b.Helper()
	results, err := mr.Results(ctx, client, prefix)
	if err != nil {
		b.Fatalf("MapReduce results: %v", err)
	}
	if len(results) != mapReduceWordKinds {
		b.Fatalf("MapReduce returned %d results, want %d", len(results), mapReduceWordKinds)
	}
	wantCount := strconv.Itoa(mapReduceDocuments * mapReduceWordRepeats)
	for word, result := range results {
		if got, want := string(result.Key), strconv.Itoa(word); got != want {
			b.Fatalf("MapReduce result %d key %q, want %q", word, got, want)
		}
		if got := string(result.Value); got != wantCount {
			b.Fatalf("MapReduce result %q count %q, want %q", result.Key, got, wantCount)
		}
	}
}

func sampleQueueStats(ctx context.Context, client *entroq.EntroQ, prefix string, interval time.Duration) (statsSamples, error) {
	var samples statsSamples
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		start := time.Now()
		stats, err := client.QueueStats(ctx, entroq.MatchPrefix(prefix))
		if err != nil {
			if ctx.Err() != nil {
				return samples, nil
			}
			return samples, err
		}
		samples.add(time.Since(start), stats)

		select {
		case <-ctx.Done():
			return samples, nil
		case <-ticker.C:
		}
	}
}

func (s *statsSamples) add(latency time.Duration, stats map[string]*entroq.QueueStat) {
	s.latencies = append(s.latencies, latency)
	var current queueHighWater
	for name, stat := range stats {
		current.queued += stat.Size
		current.claimed += stat.Claimed
		switch {
		case strings.HasSuffix(name, "/map/result"):
			current.mapResult += stat.Size
		case strings.HasSuffix(name, "/reduce/result"):
			current.reduceResult += stat.Size
		case strings.HasSuffix(name, "/map"):
			current.mapInput += stat.Size
		case strings.HasSuffix(name, "/reduce"):
			current.reduceInput += stat.Size
		}
	}
	s.highWater.queued = max(s.highWater.queued, current.queued)
	s.highWater.claimed = max(s.highWater.claimed, current.claimed)
	s.highWater.mapInput = max(s.highWater.mapInput, current.mapInput)
	s.highWater.mapResult = max(s.highWater.mapResult, current.mapResult)
	s.highWater.reduceInput = max(s.highWater.reduceInput, current.reduceInput)
	s.highWater.reduceResult = max(s.highWater.reduceResult, current.reduceResult)
}

func (s *statsSamples) report(b *testing.B, duration time.Duration, operations int) {
	b.Helper()
	if len(s.latencies) == 0 {
		b.Fatal("stats sampler collected no samples")
	}
	slices.Sort(s.latencies)
	b.ReportMetric(float64(len(s.latencies))/duration.Seconds(), "stats/s")
	b.ReportMetric(float64(len(s.latencies))/float64(operations), "stats/op")
	b.ReportMetric(float64(percentile(s.latencies, 0.50).Microseconds()), "stats-p50-µs")
	b.ReportMetric(float64(percentile(s.latencies, 0.95).Microseconds()), "stats-p95-µs")
	b.ReportMetric(float64(percentile(s.latencies, 0.99).Microseconds()), "stats-p99-µs")
	b.ReportMetric(float64(s.latencies[len(s.latencies)-1].Microseconds()), "stats-max-µs")
	b.ReportMetric(float64(s.highWater.queued), "queued-max")
	b.ReportMetric(float64(s.highWater.claimed), "claimed-max")
	b.ReportMetric(float64(s.highWater.mapInput), "map-max")
	b.ReportMetric(float64(s.highWater.mapResult), "map-result-max")
	b.ReportMetric(float64(s.highWater.reduceInput), "reduce-max")
	b.ReportMetric(float64(s.highWater.reduceResult), "reduce-result-max")
}

func percentile(values []time.Duration, quantile float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := int(float64(len(values)-1) * quantile)
	return values[index]
}
