package eqpg

import (
	"context"
	"fmt"
	"log"
	"time"
)

// Default garbage-collection tuning. These are intentionally not exposed as
// options: GC is a first-class, always-on backend behavior, and the right
// cadence/batch is a property of the storage engine, not something callers
// should have to reason about. Tests override the interval via withGCInterval.
const (
	defaultGCInterval  = time.Minute
	defaultGCBatchSize = 1000
)

// withGCInterval overrides the GC scan interval. Unexported: only in-package
// tests use it, to drive the loop fast enough to observe. There is no public
// GC configuration.
func withGCInterval(d time.Duration) PGOpt {
	return func(opts *pgOptions) {
		opts.gcInterval = d
	}
}

// collectOnce deletes up to batch collectable tasks from due gc= queues via the
// gc_collect stored procedure and returns the total number deleted. The procedure
// returns one row per affected queue; we sum for the total (which drives the
// drain loop) and report each queue's count to GC telemetry. It is one bounded,
// quickly-committed transaction; the loop calls it repeatedly to drain a backlog
// without any single statement holding locks or accumulating WAL.
func (b *EQPG) collectOnce(ctx context.Context, batch int) (int, error) {
	rows, err := b.DB.QueryContext(ctx, "SELECT queue_name, deleted FROM entroq.gc_collect($1)", batch)
	if err != nil {
		return 0, fmt.Errorf("gc_collect: %w", err)
	}
	defer rows.Close()

	total := 0
	for rows.Next() {
		var queue string
		var n int
		if err := rows.Scan(&queue, &n); err != nil {
			return total, fmt.Errorf("gc_collect scan: %w", err)
		}
		b.gcMetrics.Deleted(ctx, queue, n)
		total += n
	}
	if err := rows.Err(); err != nil {
		return total, fmt.Errorf("gc_collect rows: %w", err)
	}
	return total, nil
}

// runGCLoop drains due gc= queues on an interval until ctx is canceled. Each
// tick drains the current backlog in bounded batches -- a tight loop of
// collectOnce calls, each its own transaction that releases its row locks (via
// FOR UPDATE SKIP LOCKED) so live claims interleave freely -- then waits for the
// next tick. A full batch means more may remain, so it continues immediately; a
// short batch means the backlog is drained. Errors are logged and the loop
// continues; it never blocks the backend's own operations.
func (b *EQPG) runGCLoop(ctx context.Context, interval time.Duration, batch int) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			start := time.Now()
			for {
				n, err := b.collectOnce(ctx, batch)
				if err != nil {
					if ctx.Err() == nil {
						log.Printf("eqpg gc: %v", err)
						b.gcMetrics.Error(ctx, "", "collect")
					}
					break
				}
				if n < batch {
					break // backlog drained, or nothing was due
				}
				if ctx.Err() != nil {
					return
				}
			}
			b.gcMetrics.Sweep(ctx, time.Since(start))
		}
	}
}
