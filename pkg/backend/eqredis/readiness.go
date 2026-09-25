package eqredis

import (
	"context"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shiblon/entroq"
)

// DefaultReadinessInterval is how often the backend checks for tasks that
// become available solely through the passage of time.
const DefaultReadinessInterval = 5 * time.Second

// WithReadinessInterval sets how often the backend checks for tasks that
// became available solely through the passage of time. A non-positive interval
// disables the readiness loop, leaving claim polling as the fallback.
func WithReadinessInterval(interval time.Duration) RedisOpt {
	return func(o *redisOptions) {
		o.readinessInterval = interval
	}
}

// runReadinessLoop wakes waiters on queues that have available tasks. Only
// queues with waiters are checked, so an idle backend sends no commands.
//
// The check counts available tasks, not only newly matured ones, so it also
// finds tasks made available through another process sharing this Redis.
func (e *EQRedis) runReadinessLoop(ctx context.Context, lc entroq.ListenerCounter, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.notifyReadyQueues(ctx, lc.Listeners()); err != nil && ctx.Err() == nil {
				log.Printf("eqredis readiness: %v", err)
			}
		}
	}
}

// notifyReadyQueues notifies each waited queue once per available task, up to
// its number of waiters, using one pipelined round trip. A stale ZSET member
// whose task hash is gone can overcount; that costs only a spurious wakeup.
func (e *EQRedis) notifyReadyQueues(ctx context.Context, waiting map[string]int) error {
	if len(waiting) == 0 {
		return nil
	}
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	counts := make(map[string]*redis.IntCmd, len(waiting))
	pipe := e.client.Pipeline()
	for q := range waiting {
		counts[q] = pipe.ZCount(ctx, queueKey(q), "-inf", now)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	for q, cmd := range counts {
		for range min(int(cmd.Val()), waiting[q]) {
			e.nw.Notify(q)
		}
	}
	return nil
}
