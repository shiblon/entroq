package eqpg

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/lib/pq"
	"github.com/shiblon/entroq"
)

// DefaultReadinessInterval is how often the backend checks for tasks that
// become available solely through the passage of time.
const DefaultReadinessInterval = 5 * time.Second

// WithReadinessInterval sets how often the backend checks for tasks that
// became available solely through the passage of time. A non-positive interval
// disables the readiness loop, leaving claim polling as the fallback.
func WithReadinessInterval(interval time.Duration) PGOpt {
	return func(opts *pgOptions) {
		opts.readinessInterval = interval
	}
}

// WithHeartbeat sets the readiness interval.
//
// Deprecated: Use WithReadinessInterval. The PostgreSQL heartbeat and its
// NOTIFY broadcast were replaced by a per-backend readiness loop.
func WithHeartbeat(interval time.Duration) PGOpt {
	return WithReadinessInterval(interval)
}

// WithNoListen does nothing.
//
// Deprecated: The backend no longer uses LISTEN/NOTIFY. Changes made through
// this backend wake its claims directly, and the readiness loop finds
// everything else.
func WithNoListen() PGOpt {
	return func(*pgOptions) {}
}

// runReadinessLoop wakes waiters on queues that have available tasks. Only
// queues with waiters are checked, so an idle backend does no queries.
//
// The check counts available tasks, not only newly matured ones, so it also
// finds tasks made available by another client of the same database.
func (b *EQPG) runReadinessLoop(ctx context.Context, lc entroq.ListenerCounter, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := b.notifyReadyQueues(ctx, lc.Listeners()); err != nil && ctx.Err() == nil {
				log.Printf("eqpg readiness: %v", err)
			}
		}
	}
}

// notifyReadyQueues notifies each waited queue once per available task, up to
// its number of waiters. One query covers every queue, each counted through the
// (queue, at, claims) index and capped at its waiter count.
func (b *EQPG) notifyReadyQueues(ctx context.Context, waiting map[string]int) error {
	if len(waiting) == 0 {
		return nil
	}
	queues := make([]string, 0, len(waiting))
	limits := make([]int64, 0, len(waiting))
	for q, n := range waiting {
		queues = append(queues, q)
		limits = append(limits, int64(n))
	}

	rows, err := b.DB.QueryContext(ctx, `
		SELECT w.queue, (
			SELECT count(*) FROM (
				SELECT 1 FROM entroq.tasks t
				WHERE t.queue = w.queue AND t.at <= now()
				LIMIT w.waiters
			) ready
		)
		FROM unnest($1::text[], $2::bigint[]) AS w(queue, waiters)`,
		pq.Array(queues), pq.Array(limits))
	if err != nil {
		return fmt.Errorf("count ready tasks: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var q string
		var ready int
		if err := rows.Scan(&q, &ready); err != nil {
			return fmt.Errorf("scan ready count: %w", err)
		}
		for range ready {
			b.nw.Notify(q)
		}
	}
	return rows.Err()
}
