package eqredis

// Contention benchmark: does the single-threaded Redis server keep the hot claim
// path responsive while GC and queue/task listing run concurrently? Also measures
// WATCH/MULTI retry cost under concurrent claimers.
//
// Opt-in (it seeds a lot and runs several seconds); run with:
//   ENTROQ_REDIS_CONTENTION=1 go test ./pkg/backend/eqredis/ \
//       -run TestRedisContention -v -timeout 10m
//
// It reuses the redis:7-alpine container from TestMain (redisAddr).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

const (
	cProbeQueue   = "contend/probe"
	cProbeTasks   = 10000 // seeded into the probe queue (recycle via short claim TTL)
	cGCQueues     = 15
	cGCPerQueue   = 4000 // due gc=0 tasks per gc queue (GC interferer chews these)
	cListQueue    = "contend/listing"
	cListTasks    = 30000 // one big queue for the Tasks listing interferer
	cScenarioTime = 3 * time.Second
	cClaimers     = 8 // extra goroutines contending on the probe queue
)

func pctl(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func summarize(name string, ds []time.Duration) string {
	if len(ds) == 0 {
		return fmt.Sprintf("  %-24s (no samples)", name)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	r := func(d time.Duration) time.Duration { return d.Round(time.Microsecond) }
	return fmt.Sprintf("  %-24s n=%-7d p50=%-9v p95=%-9v p99=%-9v max=%-9v",
		name, len(ds), r(pctl(ds, 0.50)), r(pctl(ds, 0.95)), r(pctl(ds, 0.99)), r(ds[len(ds)-1]))
}

func seedContention(ctx context.Context, t *testing.T, b *EQRedis, queue string, n int) {
	t.Helper()
	val := json.RawMessage(`{"contend":true}`)
	const batch = 500
	for done := 0; done < n; {
		m := batch
		if n-done < m {
			m = n - done
		}
		args := make([]entroq.ModifyArg, m)
		for k := range args {
			args[k] = entroq.InsertingInto(queue, entroq.WithRawValue(val))
		}
		if _, err := b.Modify(ctx, entroq.NewModification("seed", args...)); err != nil {
			t.Fatalf("seed %s: %v", queue, err)
		}
		done += m
	}
}

// probe hammers TryClaim on the probe queue for d, returning per-call latencies.
// A short claim TTL lets tasks recycle so the queue never runs dry.
func probe(ctx context.Context, b *EQRedis, d time.Duration) []time.Duration {
	lats := make([]time.Duration, 0, 100000)
	claimant := "probe-" + entroq.GenHex16()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		start := time.Now()
		_, _ = b.TryClaim(ctx, &entroq.ClaimQuery{
			Queues:   []string{cProbeQueue},
			Claimant: claimant,
			Duration: 2 * time.Second,
		})
		lats = append(lats, time.Since(start))
	}
	return lats
}

func TestRedisContention(t *testing.T) {
	if os.Getenv("ENTROQ_REDIS_CONTENTION") == "" {
		t.Skip("set ENTROQ_REDIS_CONTENTION=1 to run the contention benchmark")
	}
	ctx := context.Background()

	// GC loop off (1h) so we drive GC explicitly.
	b, err := Open(ctx, WithAddr(redisAddr), withGCInterval(time.Hour))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer b.Close()

	t.Logf("seeding: probe=%d, gc=%dx%d, listing=%d ...", cProbeTasks, cGCQueues, cGCPerQueue, cListTasks)
	seedStart := time.Now()
	seedContention(ctx, t, b, cProbeQueue, cProbeTasks)
	seedContention(ctx, t, b, cListQueue, cListTasks)
	for i := 0; i < cGCQueues; i++ {
		seedContention(ctx, t, b, fmt.Sprintf("contend/gc/%d/gc=0", i), cGCPerQueue)
	}
	t.Logf("seeded in %v", time.Since(seedStart).Round(time.Millisecond))

	// interferer launches an interferer goroutine; returns a stop func.
	interferer := func(fn func(ctx context.Context)) func() {
		ictx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ictx.Err() == nil {
				fn(ictx)
			}
		}()
		return func() { cancel(); wg.Wait() }
	}

	claimContenders := func() func() {
		ictx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		for i := 0; i < cClaimers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				c := fmt.Sprintf("contender-%d", i)
				for ictx.Err() == nil {
					_, _ = b.TryClaim(ictx, &entroq.ClaimQuery{Queues: []string{cProbeQueue}, Claimant: c, Duration: 2 * time.Second})
				}
			}(i)
		}
		return func() { cancel(); wg.Wait() }
	}

	gcInterferer := func(ctx context.Context) { _, _ = b.collectOnce(ctx, 1000) }
	// Intermittent: one full listing, then a gap the hot path can use. Continuous
	// (gapless) listing is a pessimal case that saturates the server regardless of
	// chunking; real listings arrive periodically.
	listInterferer := func(ctx context.Context) {
		_, _ = b.Tasks(ctx, &entroq.TasksQuery{Queue: cListQueue})
		_, _ = b.Queues(ctx, &entroq.QueuesQuery{})
		select {
		case <-ctx.Done():
		case <-time.After(15 * time.Millisecond):
		}
	}

	t.Log("=== TryClaim latency by scenario (probe queue) ===")

	// 1. Baseline.
	t.Log(summarize("baseline", probe(ctx, b, cScenarioTime)))

	// 2. Claim contention (WATCH/MULTI retries).
	stop := claimContenders()
	t.Log(summarize(fmt.Sprintf("+%d claimers", cClaimers), probe(ctx, b, cScenarioTime)))
	stop()

	// 3. GC running.
	stop = interferer(gcInterferer)
	t.Log(summarize("+GC", probe(ctx, b, cScenarioTime)))
	stop()

	// 4. Intermittent listing (Tasks on a big queue + Queues).
	stop = interferer(listInterferer)
	t.Log(summarize("+listing(Tasks+Queues)", probe(ctx, b, cScenarioTime)))
	stop()

	// 5. Everything at once.
	stops := []func(){claimContenders(), interferer(gcInterferer), interferer(listInterferer)}
	t.Log(summarize("+claimers+GC+listing", probe(ctx, b, cScenarioTime)))
	for _, s := range stops {
		s()
	}
}
