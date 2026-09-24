package eqmem

import (
	"context"
	"time"

	"github.com/shiblon/entroq"
)

const defaultReadinessInterval = 5 * time.Second

// WithReadinessInterval sets how often the backend checks for tasks that
// became available solely through the passage of time. A non-positive interval
// disables the readiness loop, leaving claim polling as the fallback.
func WithReadinessInterval(interval time.Duration) Option {
	return func(m *EQMem) {
		m.readinessInterval = interval
	}
}

// runReadinessLoop notifies queues whose earliest task has become available.
func (m *EQMem) runReadinessLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.notifyReadyQueues(entroq.ProcessTime())
		}
	}
}

func (m *EQMem) notifyReadyQueues(now time.Time) {
	for _, ql := range m.snapshotQueueLocks() {
		ql.Lock()
		ready := ql.heap.Len() > 0 && !now.Before(ql.heap.Top().at)
		ql.Unlock()

		// Notify after releasing the queue lock so a woken claimant can acquire
		// it immediately.
		if ready {
			m.nw.Notify(ql.queue)
		}
	}
}
