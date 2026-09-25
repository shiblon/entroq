package eqsqlite

import (
	"context"
	"log"
	"time"

	"github.com/shiblon/entroq"
)

// DefaultReadinessInterval is how often the backend checks for tasks that
// become available solely through the passage of time.
const DefaultReadinessInterval = 5 * time.Second

// WithReadinessInterval sets how often the backend checks for tasks that
// became available solely through the passage of time. A non-positive interval
// disables the readiness loop, leaving claim polling as the fallback.
func WithReadinessInterval(interval time.Duration) Option {
	return func(o *options) { o.readinessInterval = interval }
}

// runReadinessLoop wakes waiters on queues that have available tasks. Only
// queues with waiters are checked, so an idle backend does no reads.
func (b *EQSQLite) runReadinessLoop(ctx context.Context, lc entroq.ListenerCounter, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := b.notifyReadyQueues(ctx, lc.Listeners()); err != nil && ctx.Err() == nil {
				log.Printf("eqsqlite readiness: %v", err)
			}
		}
	}
}

// notifyReadyQueues notifies each waited queue once per available task, up to
// its number of waiters.
func (b *EQSQLite) notifyReadyQueues(ctx context.Context, waiting map[string]int) error {
	if len(waiting) == 0 {
		return nil
	}
	stmt, err := b.readDB.PrepareContext(ctx,
		"SELECT COUNT(*) FROM (SELECT 1 FROM tasks WHERE queue = ? AND at_ms <= ? LIMIT ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	nowMs := nowUTC().UnixMilli()
	for q, waiters := range waiting {
		var ready int
		if err := stmt.QueryRowContext(ctx, q, nowMs, waiters).Scan(&ready); err != nil {
			return err
		}
		for range ready {
			b.nw.Notify(q)
		}
	}
	return nil
}
