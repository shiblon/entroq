package eqmem

import (
	"context"
	"log"
	"time"

	backendgc "github.com/shiblon/entroq/pkg/backend/internal/gc"
)

// Default garbage-collection tuning. Not exposed as options: GC is a first-class,
// always-on backend behavior. Tests override the interval via withGCInterval.
const (
	defaultGCInterval  = time.Minute
	defaultGCBatchSize = 1000
)

// withGCInterval overrides the GC scan interval. Unexported: only in-package
// tests use it, to drive the loop fast enough to observe.
func withGCInterval(d time.Duration) Option {
	return func(m *EQMem) {
		m.gcInterval = d
	}
}

// runGCLoop performs a bounded collection pass for opted-in task queues and doc
// keys on each interval until ctx is canceled.
func (m *EQMem) runGCLoop(ctx context.Context, interval time.Duration, batch int) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			start := time.Now()
			if _, err := m.collectOnce(ctx, batch); err != nil && ctx.Err() == nil {
				log.Printf("eqmem task gc: %v", err)
			}
			if _, err := m.collectDocsOnce(ctx, batch); err != nil && ctx.Err() == nil {
				log.Printf("eqmem doc gc: %v", err)
			}
			m.gcMetrics.Sweep(ctx, time.Since(start))
		}
	}
}

func (m *EQMem) collectDocsOnce(ctx context.Context, batch int) (int, error) {
	return backendgc.CollectDocsOnce(ctx, m, batch, m.gcMetrics)
}

func (m *EQMem) collectOnce(ctx context.Context, batch int) (int, error) {
	return backendgc.CollectTasksOnce(ctx, m, batch, m.gcMetrics)
}
