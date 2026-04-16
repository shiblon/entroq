package eqredis

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

const gcInterval = 5 * time.Second

// RunGC starts a background goroutine that periodically:
//  1. Scans eq:qs for queues whose ZSET is empty and removes them.
//  2. Scans eq:inflight:{name} for tasks whose ZSET score is <= now
//     (claim expired) and removes them from the inflight set.
//
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
	now := time.Now().UTC()
	nowMs := now.UnixMilli()

	queues, err := e.client.SMembers(ctx, queuesKey).Result()
	if err != nil {
		return err
	}

	for _, q := range queues {
		qKey := queueKey(q)
		ifKey := inflightKey(q)

		// Remove expired entries from inflight: tasks whose ZSET score <= now.
		members, err := e.client.SMembers(ctx, ifKey).Result()
		if err != nil {
			continue
		}
		if len(members) > 0 {
			pipe := e.client.Pipeline()
			scoreCmds := make(map[string]*redis.FloatCmd, len(members))
			for _, id := range members {
				scoreCmds[id] = pipe.ZScore(ctx, qKey, id)
			}
			if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
				continue
			}
			for id, cmd := range scoreCmds {
				score, err := cmd.Result()
				if errors.Is(err, redis.Nil) {
					// Task no longer in ZSET at all -- remove from inflight.
					e.client.SRem(ctx, ifKey, id)
					continue
				}
				if err != nil {
					continue
				}
				if int64(score) <= nowMs {
					// Claim expired. Note: if the task was re-claimed between
					// our ZSCORE read and this SREM, we'll incorrectly evict
					// it from inflight. Impact is a briefly-wrong Claimed count
					// in QueueStats; correctness is unaffected since claim and
					// modify operate on ZSET scores, not the inflight set.
					e.client.SRem(ctx, ifKey, id)
				}
			}
		}

		// Remove empty queues from eq:qs.
		// Redis auto-removes empty ZSETs, so we only clean up the bookkeeping
		// set. Deleting qKey would race with concurrent inserts and could
		// destroy a task that arrived between our ZCARD and the DEL.
		size, err := e.client.ZCard(ctx, qKey).Result()
		if err != nil {
			continue
		}
		if size == 0 {
			e.client.SRem(ctx, queuesKey, q)
		}
	}
	return nil
}
