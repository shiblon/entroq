package eqsqlite

import (
	"context"
	"log"
	"time"

	backendgc "github.com/shiblon/entroq/pkg/backend/internal/gc"
)

const (
	defaultGCInterval  = 5 * time.Second
	defaultGCBatchSize = 1000
)

func withGCInterval(d time.Duration) Option {
	return func(o *options) { o.gcInterval = d }
}

func (b *EQSQLite) runGCLoop(ctx context.Context, interval time.Duration, batch int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			start := time.Now()
			if _, err := b.collectOnce(ctx, batch); err != nil && ctx.Err() == nil {
				log.Printf("eqsqlite gc collect tasks: %v", err)
			}
			if _, err := b.collectDocsOnce(ctx, batch); err != nil && ctx.Err() == nil {
				log.Printf("eqsqlite gc collect docs: %v", err)
			}
			b.gcMetrics.Sweep(ctx, time.Since(start))
		}
	}
}

func (b *EQSQLite) collectOnce(ctx context.Context, batch int) (int, error) {
	return backendgc.CollectTasksOnce(ctx, b, batch, b.gcMetrics)
}

func (b *EQSQLite) collectDocsOnce(ctx context.Context, batch int) (int, error) {
	return backendgc.CollectDocsOnce(ctx, b, batch, b.gcMetrics)
}
