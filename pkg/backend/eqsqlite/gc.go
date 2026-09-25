package eqsqlite

import (
	"context"
	"database/sql"
	"fmt"
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
			if _, err := b.collectLocksOnce(ctx, batch); err != nil && ctx.Err() == nil {
				log.Printf("eqsqlite gc collect doc locks: %v", err)
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

// collectLocksOnce removes the locks of up to batch doc groups that have no
// docs and are not held, so claimed-then-abandoned groups do not accumulate.
// Writes are serialized, so no insert can join a group between the check and
// the delete.
func (b *EQSQLite) collectLocksOnce(ctx context.Context, batch int) (int, error) {
	value, err := b.write(ctx, func(ctx context.Context, tx *sql.Tx) (any, error) {
		res, err := tx.ExecContext(ctx, `DELETE FROM doc_locks WHERE rowid IN (
			SELECT l.rowid FROM doc_locks l
			WHERE (l.claimant = '' OR l.at_ms <= ?)
			  AND NOT EXISTS (SELECT 1 FROM docs d WHERE d.namespace = l.namespace AND d.key_primary = l.key_primary)
			LIMIT ?)`, nowUTC().UnixMilli(), batch)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		return int(n), err
	})
	if err != nil {
		return 0, fmt.Errorf("eqsqlite collect doc locks: %w", err)
	}
	return value.(int), nil
}
