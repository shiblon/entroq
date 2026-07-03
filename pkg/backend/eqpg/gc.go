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
// gc_collect stored procedure and returns the number deleted. It is one bounded,
// quickly-committed transaction; the loop calls it repeatedly to drain a backlog
// without any single statement holding locks or accumulating WAL.
func (b *EQPG) collectOnce(ctx context.Context, batch int) (int, error) {
	var n int
	if err := b.DB.QueryRowContext(ctx, "SELECT entroq.gc_collect($1)", batch).Scan(&n); err != nil {
		return 0, fmt.Errorf("gc_collect: %w", err)
	}
	return n, nil
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
			for {
				n, err := b.collectOnce(ctx, batch)
				if err != nil {
					if ctx.Err() == nil {
						log.Printf("eqpg gc: %v", err)
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
		}
	}
}
