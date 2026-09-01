package eqtest

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/examples/mrtest"
)

const mapReduceStatsInterval = 10 * time.Millisecond

type mapReduceStatsResult struct {
	polls        int
	observedWork bool
	err          error
}

// MapReduce exercises the task, worker, document, and queue-stats operations
// needed to run a concurrent MapReduce pipeline and verifies its final results.
func MapReduce(ctx context.Context, t *testing.T, client *entroq.EntroQ, _ string) {
	t.Helper()

	const (
		numDocs     = 100
		numMappers  = 8
		numReducers = 3
	)
	prefix := "/mrtest/" + entroq.GenHex16()
	statsCtx, cancelStats := context.WithCancel(ctx)
	statsReady := make(chan error, 1)
	statsDone := make(chan mapReduceStatsResult, 1)
	go func() {
		statsDone <- pollMapReduceStats(statsCtx, client, prefix, statsReady)
	}()
	if err := <-statsReady; err != nil {
		cancelStats()
		<-statsDone
		t.Fatalf("initial MapReduce queue stats: %v", err)
	}

	resultsOK := mrtest.MRCheckAt(ctx, client, prefix, numDocs, numMappers, numReducers)
	cancelStats()
	statsResult := <-statsDone
	if !resultsOK {
		t.Error("MapReduce pipeline returned incorrect results")
	}
	if statsResult.err != nil {
		t.Errorf("poll MapReduce queue stats: %v", statsResult.err)
	}
	if statsResult.polls < 2 {
		t.Errorf("MapReduce queue stats polled %d times, want at least 2", statsResult.polls)
	}
	if !statsResult.observedWork {
		t.Error("MapReduce queue stats never observed queued or claimed work")
	}
}

func pollMapReduceStats(
	ctx context.Context,
	client *entroq.EntroQ,
	prefix string,
	ready chan<- error,
) mapReduceStatsResult {
	var result mapReduceStatsResult
	stats, err := client.QueueStats(ctx, entroq.MatchPrefix(prefix))
	ready <- err
	if err != nil {
		result.err = err
		return result
	}
	result.add(stats)

	ticker := time.NewTicker(mapReduceStatsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return result
		case <-ticker.C:
		}
		stats, err := client.QueueStats(ctx, entroq.MatchPrefix(prefix))
		if err != nil {
			if ctx.Err() != nil {
				return result
			}
			result.err = err
			return result
		}
		result.add(stats)
	}
}

func (r *mapReduceStatsResult) add(stats map[string]*entroq.QueueStat) {
	r.polls++
	for _, stat := range stats {
		if stat.Size > 0 || stat.Claimed > 0 {
			r.observedWork = true
			return
		}
	}
}
