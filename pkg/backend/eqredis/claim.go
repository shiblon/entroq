package eqredis

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shiblon/entroq"
)

const maxClaimRetries = 50

// TryClaim attempts to claim a task from one of the queues in cq.
// Returns nil, nil if no claimable task is found.
func (e *EQRedis) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	now := time.Now().UTC()

	// Shuffle queues to avoid consistently favoring one; uniform selection probability.
	queues := make([]string, len(cq.Queues))
	copy(queues, cq.Queues)
	rand.Shuffle(len(queues), func(i, j int) {
		queues[i], queues[j] = queues[j], queues[i]
	})

	for _, q := range queues {
		task, err := e.tryClaimOne(ctx, q, cq.Claimant, cq.Duration, now)
		if err != nil {
			return nil, fmt.Errorf("try claim %q: %w", q, err)
		}
		if task != nil {
			return task, nil
		}
	}
	return nil, nil
}

// tryClaimOne attempts to claim a single task from the given queue.
// Returns nil, nil if no task is available.
func (e *EQRedis) tryClaimOne(ctx context.Context, queue string, claimant string, duration time.Duration, now time.Time) (*entroq.Task, error) {
	qKey := queueKey(queue)
	ifKey := inflightKey(queue)
	nowMs := now.UnixMilli()
	newAtMs := now.Add(duration).UnixMilli()

	for range maxClaimRetries {
		// Get claimable candidates: tasks with at <= now.
		candidates, err := e.client.ZRangeArgs(ctx, redis.ZRangeArgs{
			Key:     qKey,
			Start:   "0",
			Stop:    fmt.Sprintf("%d", nowMs),
			ByScore: true,
			Offset:  0,
			Count:   claimWindow,
		}).Result()
		if err != nil {
			return nil, fmt.Errorf("zrangebyscore %q: %w", queue, err)
		}
		if len(candidates) == 0 {
			return nil, nil
		}

		// Pick a random candidate to spread collisions.
		id := candidates[rand.Intn(len(candidates))]
		tKey := taskKey(id)

		// Optimistic claim: WATCH the task hash, verify it is still claimable,
		// then atomically update it.
		var claimed *taskFields
		err = e.client.Watch(ctx, func(tx *redis.Tx) error {
			vals, err := tx.HGetAll(ctx, tKey).Result()
			if err != nil {
				return fmt.Errorf("hgetall %q: %w", id, err)
			}
			if len(vals) == 0 {
				// Task was deleted between the ZRANGEBYSCORE and now; retry.
				return nil
			}

			f, err := parseTaskFields(vals)
			if err != nil {
				return fmt.Errorf("parse task %q: %w", id, err)
			}

			// Re-check claimability: at must still be <= now.
			if f.AtMs > nowMs {
				// Another client already claimed it (at was pushed into the future).
				return nil
			}

			f.Claimant = string(claimant)
			f.AtMs = newAtMs
			f.Version++
			f.Claims++
			f.Modified = nowMs

			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.HSet(ctx, tKey, f.toMap())
				pipe.ZAdd(ctx, qKey, redis.Z{Score: float64(newAtMs), Member: id})
				pipe.SAdd(ctx, ifKey, id)
				pipe.SAdd(ctx, queuesKey, queue)
				return nil
			})
			if err != nil {
				return err
			}

			claimed = f
			return nil
		}, tKey)

		if claimed != nil {
			return claimed.toTask(), nil
		}

		if err != nil && !errors.Is(err, redis.TxFailedErr) {
			return nil, fmt.Errorf("watch/exec claim %q: %w", id, err)
		}
	}
	return nil, fmt.Errorf("claim %q: exceeded %d retries, possible contention or bug", queue, maxClaimRetries)
}

// Claim blocks until a task is claimed or ctx is canceled.
func (e *EQRedis) Claim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	return entroq.WaitTryClaim(ctx, cq, e.TryClaim, e.nw)
}
