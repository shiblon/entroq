package eqredis

import (
	"context"
	"log"
	"time"
)

const gcInterval = 5 * time.Second

// RunGC starts a background goroutine that periodically removes empty queues
// from {eq}:qs and empty namespaces from {eq}:ns.
// Runs until ctx is canceled. Errors are logged and the loop continues.
func (e *EQRedis) RunGC(ctx context.Context) {
	go func() {
		t := time.NewTicker(gcInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := e.gc(ctx); err != nil {
					log.Printf("eqredis gc: %v", err)
				}
			}
		}
	}()
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
