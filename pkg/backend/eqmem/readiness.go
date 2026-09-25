package eqmem

import (
	"context"
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
	// With waiter counts, check only waited queues and wake one waiter per
	// available task. A notifier that cannot count waiters gets one wakeup per
	// queue whose earliest task is available.
	var listeners map[string]int
	lc, counting := m.nw.(entroq.ListenerCounter)
	if counting {
		if listeners = lc.Listeners(); len(listeners) == 0 {
			return
		}
	}

	for _, ql := range m.snapshotQueueLocks() {
		limit := 1
		if counting {
			if limit = listeners[ql.queue]; limit == 0 {
				continue
			}
		}
		ql.Lock()
		ready := ql.heap.CountAvailable(now, limit)
		ql.Unlock()

		// Notify after releasing the queue lock so a woken claimant can acquire
		// it immediately.
		for range ready {
			m.nw.Notify(ql.queue)
		}
	}
}
