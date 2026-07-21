package eqredis

import (
	"context"
	"log"
	"time"

	backendgc "github.com/shiblon/entroq/pkg/backend/internal/gc"
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

// runGCLoop performs one bounded task/doc collection pass per tick, then removes
// empty queue and namespace bookkeeping entries.
func (e *EQRedis) runGCLoop(ctx context.Context, interval time.Duration, batch int) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			start := time.Now()
			if _, err := e.collectOnce(ctx, batch); err != nil && ctx.Err() == nil {
				log.Printf("eqredis gc collect tasks: %v", err)
			}
			if _, err := e.collectDocsOnce(ctx, batch); err != nil && ctx.Err() == nil {
				log.Printf("eqredis gc collect docs: %v", err)
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

func (e *EQRedis) collectDocsOnce(ctx context.Context, batch int) (int, error) {
	return backendgc.CollectDocsOnce(ctx, e, batch, e.gcMetrics)
}

func (e *EQRedis) collectOnce(ctx context.Context, batch int) (int, error) {
	return backendgc.CollectTasksOnce(ctx, e, batch, e.gcMetrics)
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
