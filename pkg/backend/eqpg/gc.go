package eqpg

import (
	"context"
	"log"
	"time"

	backendgc "github.com/shiblon/entroq/pkg/backend/internal/gc"
)

const (
	defaultGCInterval  = time.Minute
	defaultGCBatchSize = 1000
)

func withGCInterval(d time.Duration) PGOpt {
	return func(opts *pgOptions) {
		opts.gcInterval = d
	}
}

func (b *EQPG) runGCLoop(ctx context.Context, interval time.Duration, batch int) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.gcSweep(ctx, batch)
		}
	}
}

func (b *EQPG) gcSweep(ctx context.Context, batch int) {
	start := time.Now()
	if _, err := b.collectOnce(ctx, batch); err != nil && ctx.Err() == nil {
		log.Printf("eqpg task gc: %v", err)
	}
	if _, err := b.collectDocsOnce(ctx, batch); err != nil && ctx.Err() == nil {
		log.Printf("eqpg doc gc: %v", err)
	}
	b.gcMetrics.Sweep(ctx, time.Since(start))
}

func (b *EQPG) collectOnce(ctx context.Context, batch int) (int, error) {
	return backendgc.CollectTasksOnce(ctx, b, batch, b.gcMetrics)
}

func (b *EQPG) collectDocsOnce(ctx context.Context, batch int) (int, error) {
	return backendgc.CollectDocsOnce(ctx, b, batch, b.gcMetrics)
}
