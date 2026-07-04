package eqpg

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/lib/pq"
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

// gcQueue is one row of entroq.gc_queues: a queue that opted into GC, with its
// activation time. activateAt.Valid == false means the gc= value is malformed
// (the queue opted in but its timestamp will not parse).
type gcQueue struct {
	queue      string
	activateAt sql.NullTime
}

// gcQueues runs the mandatory discovery step: it returns every gc= queue with
// its activation time (or a NULL activation for a malformed value). The grammar
// lives entirely in the gc_queues/gc_activation SQL; this side never parses.
func (b *EQPG) gcQueues(ctx context.Context) ([]gcQueue, error) {
	rows, err := b.DB.QueryContext(ctx, "SELECT queue, activate_at FROM entroq.gc_queues()")
	if err != nil {
		return nil, fmt.Errorf("gc_queues: %w", err)
	}
	defer rows.Close()

	var out []gcQueue
	for rows.Next() {
		var q gcQueue
		if err := rows.Scan(&q.queue, &q.activateAt); err != nil {
			return out, fmt.Errorf("gc_queues scan: %w", err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("gc_queues rows: %w", err)
	}
	return out, nil
}

// collectOnce deletes up to batch collectable tasks from the given queues (whose
// activation has passed) via the gc_collect stored procedure, and returns the
// total number deleted. The procedure returns one row per affected queue; we sum
// for the total (which drives the drain loop) and report each queue's count to GC
// telemetry. It is one bounded, quickly-committed transaction; the loop calls it
// repeatedly to drain a backlog without any single statement holding locks or
// accumulating WAL. gc_collect ignores any queue whose supplied activation is in
// the future, so passing the full valid set each call is fine.
func (b *EQPG) collectOnce(ctx context.Context, queues []string, activations []time.Time, batch int) (int, error) {
	rows, err := b.DB.QueryContext(ctx,
		"SELECT queue_name, deleted FROM entroq.gc_collect($1, $2, $3)",
		pq.Array(queues), pq.Array(activations), batch)
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

// runGCLoop garbage-collects gc= queues on an interval until ctx is canceled.
// Each tick is one sweep: discover the gc= queues once, surface any malformed
// ones, then drain the due queues in bounded batches. Errors are logged and the
// loop continues; it never blocks the backend's own operations.
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

// gcSweep runs a single GC pass: discover, report malformed, drain due queues.
func (b *EQPG) gcSweep(ctx context.Context, batch int) {
	start := time.Now()

	qs, err := b.gcQueues(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("eqpg gc: %v", err)
			b.gcMetrics.Error(ctx, "", "discover")
		}
		return
	}

	// Partition into due-able (valid activation) and malformed (NULL activation).
	// Malformed queues are reported, never collected; valid ones are relayed to
	// gc_collect, which itself skips those whose activation is still in the future.
	var queues []string
	var activations []time.Time
	for _, q := range qs {
		if !q.activateAt.Valid {
			b.gcMetrics.Error(ctx, q.queue, "malformed")
			log.Printf("eqpg gc: queue %q has a malformed gc= value; it will never be collected", q.queue)
			continue
		}
		queues = append(queues, q.queue)
		activations = append(activations, q.activateAt.Time)
	}

	// Drain the due queues in bounded batches.
	for len(queues) > 0 {
		n, err := b.collectOnce(ctx, queues, activations, batch)
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
