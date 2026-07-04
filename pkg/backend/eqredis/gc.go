package eqredis

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shiblon/entroq/pkg/queues"
)

// Default garbage-collection tuning. Not exposed as options: GC is a first-class,
// always-on backend behavior, tuned to the storage engine. Tests override the
// interval via withGCInterval.
const (
	defaultGCInterval  = 5 * time.Second
	defaultGCBatchSize = 1000
)

// withGCInterval overrides the GC scan interval. Unexported: only in-package
// tests use it, to drive the loop fast enough to observe.
func withGCInterval(d time.Duration) RedisOpt {
	return func(o *redisOptions) {
		o.gcInterval = d
	}
}

// runGCLoop periodically drains queues that opt into garbage collection by name
// (a /gc= component) and removes now-empty bookkeeping entries, until ctx is
// canceled. Each tick drains due gc= tasks in bounded batches, then sweeps empty
// queues/namespaces. Errors are logged and the loop continues.
func (e *EQRedis) runGCLoop(ctx context.Context, interval time.Duration, batch int) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			start := time.Now()
			for {
				n, err := e.collectOnce(ctx, batch)
				if err != nil {
					if ctx.Err() == nil {
						log.Printf("eqredis gc collect: %v", err)
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
			if err := e.gc(ctx); err != nil {
				if ctx.Err() == nil {
					log.Printf("eqredis gc cleanup: %v", err)
					e.gcMetrics.Error(ctx, "", "cleanup")
				}
			}
			e.gcMetrics.Sweep(ctx, time.Since(start))
		}
	}
}

// collectOnce deletes up to batch collectable tasks across queues whose gc
// activation has passed, and returns the number deleted. Discovery uses the Go
// parser (queues.GCActivation) over the active-queue set, so the gc= grammar
// lives in exactly one place; only the delete is Redis-specific.
func (e *EQRedis) collectOnce(ctx context.Context, batch int) (int, error) {
	now := time.Now().UTC()
	nowMs := now.UnixMilli()

	names, err := e.client.SMembers(ctx, queuesKey).Result()
	if err != nil {
		if ctx.Err() == nil { // don't count shutdown cancellation as a GC error
			e.gcMetrics.Error(ctx, "", "list")
		}
		return 0, fmt.Errorf("gc list queues: %w", err)
	}

	total := 0
	for _, q := range names {
		if total >= batch {
			break
		}
		at, present, err := queues.GCActivation(q)
		if err != nil || !present || at.After(now) {
			continue // not a gc= queue, malformed, or not yet due
		}
		n, err := e.collectQueue(ctx, q, nowMs, batch-total)
		if err != nil {
			if ctx.Err() == nil {
				e.gcMetrics.Error(ctx, q, "collect")
			}
			return total, fmt.Errorf("gc collect %q: %w", q, err)
		}
		e.gcMetrics.Deleted(ctx, q, n)
		total += n
	}
	return total, nil
}

// collectQueue deletes up to limit arrived (score = at <= nowMs) tasks from a
// single gc= queue, atomically. It WATCHes the queue ZSET: if a concurrent
// claim, insert, or delete changes it between the read and the EXEC, the
// transaction aborts and nothing is deleted this pass (best-effort; the next
// tick retries). A claim pushes a task's score into the future, so it either
// falls outside the range read here or trips the WATCH -- a claimed task is
// never collected.
func (e *EQRedis) collectQueue(ctx context.Context, q string, nowMs int64, limit int) (int, error) {
	qKey := queueKey(q)
	deleted := 0
	err := e.client.Watch(ctx, func(tx *redis.Tx) error {
		ids, err := tx.ZRangeArgs(ctx, redis.ZRangeArgs{
			Key:     qKey,
			Start:   "0",
			Stop:    strconv.FormatInt(nowMs, 10),
			ByScore: true,
			Offset:  0,
			Count:   int64(limit),
		}).Result()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, id := range ids {
				pipe.Del(ctx, taskKey(id))
				pipe.ZRem(ctx, qKey, id)
				pipe.ZRem(ctx, qsclaimedKey(q), id)
			}
			return nil
		})
		if err != nil {
			return err
		}
		deleted = len(ids)
		return nil
	}, qKey)

	if errors.Is(err, redis.TxFailedErr) {
		return 0, nil // concurrent change; retry on the next tick
	}
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

func (e *EQRedis) gc(ctx context.Context) error {
	// Remove empty queues from {eq}:qs.
	// Redis auto-removes empty ZSETs, so we only clean up the bookkeeping set.
	queues, err := e.client.SMembers(ctx, queuesKey).Result()
	if err != nil {
		return err
	}
	for _, q := range queues {
		size, err := e.client.ZCard(ctx, queueKey(q)).Result()
		if err != nil {
			continue
		}
		if size == 0 {
			e.client.SRem(ctx, queuesKey, q)
		}
	}

	// Remove empty namespaces from {eq}:ns.
	namespaces, err := e.client.SMembers(ctx, namespacesKey).Result()
	if err != nil {
		return err
	}
	for _, ns := range namespaces {
		size, err := e.client.ZCard(ctx, docNSIndexKey(ns)).Result()
		if err != nil {
			continue
		}
		if size == 0 {
			e.client.SRem(ctx, namespacesKey, ns)
		}
	}
	return nil
}
