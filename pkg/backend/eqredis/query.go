package eqredis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shiblon/entroq"
)

// matchesQueuesQuery returns true if the queue name passes the filter in qq.
func matchesQueuesQuery(name string, qq *entroq.QueuesQuery) bool {
	if len(qq.MatchExact) > 0 {
		for _, e := range qq.MatchExact {
			if name == e {
				return true
			}
		}
		return false
	}
	if len(qq.MatchPrefix) > 0 {
		for _, p := range qq.MatchPrefix {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	return true
}

// QueueStats returns per-queue statistics.
//
// Stats are computed from the ZSET and inflight Set without reading task
// hashes, so they are O(Q) in the number of matching queues:
//
//   Size      = ZCARD eq:q:{name}
//   Claimed   = SCARD eq:inflight:{name}
//   Available = ZCOUNT eq:q:{name} 0 now_ms
//               (claimed tasks have score > now by definition, so this is exact)
//   Future    = Size - Available - Claimed
//               (approximation: includes expired-but-rescheduled tasks with claims>0)
//   MaxClaims = 0 (always; see TestQueueStats in redis_test.go)
//
// MaxClaims requires reading every task hash in each queue -- O(tasks), not
// O(queues). Postgres computes this via an index-only scan and does not block
// writers; Redis has no equivalent. Accepting the weaker contract here rather
// than introducing a hot-path linear scan on a stats call.
func (e *EQRedis) QueueStats(ctx context.Context, qq *entroq.QueuesQuery) (map[string]*entroq.QueueStat, error) {
	now := time.Now().UTC()
	nowMs := now.UnixMilli()

	// Get all known queue names.
	allQueues, err := e.client.SMembers(ctx, queuesKey).Result()
	if err != nil {
		return nil, fmt.Errorf("queue stats smembers: %w", err)
	}

	// Filter by query.
	var names []string
	for _, q := range allQueues {
		if matchesQueuesQuery(q, qq) {
			names = append(names, q)
		}
	}
	if qq.Limit > 0 && len(names) > qq.Limit {
		names = names[:qq.Limit]
	}

	if len(names) == 0 {
		return map[string]*entroq.QueueStat{}, nil
	}

	// Pipeline ZCARD, ZCOUNT (available), and SCARD (inflight) for all queues.
	pipe := e.client.Pipeline()
	zcardCmds := make(map[string]*redis.IntCmd, len(names))
	zcountCmds := make(map[string]*redis.IntCmd, len(names))
	scardCmds := make(map[string]*redis.IntCmd, len(names))
	nowStr := fmt.Sprintf("%d", nowMs)

	for _, name := range names {
		zcardCmds[name] = pipe.ZCard(ctx, queueKey(name))
		zcountCmds[name] = pipe.ZCount(ctx, queueKey(name), "0", nowStr)
		scardCmds[name] = pipe.SCard(ctx, inflightKey(name))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("queue stats pipeline: %w", err)
	}

	result := make(map[string]*entroq.QueueStat, len(names))
	for _, name := range names {
		size := int(zcardCmds[name].Val())
		if size == 0 {
			// Queue is empty; skip and let GC clean it up.
			continue
		}
		available := int(zcountCmds[name].Val())
		claimed := int(scardCmds[name].Val())
		future := size - available - claimed
		if future < 0 {
			future = 0
		}
		result[name] = &entroq.QueueStat{
			Name:      name,
			Size:      size,
			Claimed:   claimed,
			Available: available,
			Future:    future,
			MaxClaims: 0,
		}
	}
	return result, nil
}

// Queues returns a mapping from queue names to task counts.
func (e *EQRedis) Queues(ctx context.Context, qq *entroq.QueuesQuery) (map[string]int, error) {
	return entroq.QueuesFromStats(e.QueueStats(ctx, qq))
}

// Tasks retrieves tasks from a queue, optionally filtered by claimant or IDs.
func (e *EQRedis) Tasks(ctx context.Context, tq *entroq.TasksQuery) ([]*entroq.Task, error) {
	now := time.Now().UTC()

	// Collect task IDs to fetch.
	var ids []string

	if len(tq.IDs) > 0 {
		ids = tq.IDs
	} else if tq.Queue != "" {
		// Get all task IDs in the queue, ordered by at.
		members, err := e.client.ZRange(ctx, queueKey(tq.Queue), 0, -1).Result()
		if err != nil {
			return nil, fmt.Errorf("tasks zrange %q: %w", tq.Queue, err)
		}
		ids = members
	} else {
		// No queue and no IDs: not supported efficiently without a full keyspace scan.
		return nil, fmt.Errorf("tasks: queue or IDs must be specified")
	}

	if len(ids) == 0 {
		return nil, nil
	}

	// Apply limit before fetching hashes.
	if tq.Limit > 0 && len(ids) > tq.Limit {
		ids = ids[:tq.Limit]
	}

	// Pipeline HGETALL for all task IDs.
	pipe := e.client.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, taskKey(id))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("tasks pipeline hgetall: %w", err)
	}

	var tasks []*entroq.Task
	for i, cmd := range cmds {
		vals, err := cmd.Result()
		if errors.Is(err, redis.Nil) || len(vals) == 0 {
			// Task was deleted between ZRANGE and now; skip.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("tasks hgetall %q: %w", ids[i], err)
		}

		f, err := parseTaskFields(vals)
		if err != nil {
			return nil, fmt.Errorf("tasks parse %q: %w", ids[i], err)
		}

		// Filter by claimant if requested: return tasks either expired
		// or owned by this claimant.
		if tq.Claimant != "" {
			isExpired := !now.Before(time.UnixMilli(f.AtMs).UTC())
			isOwned := f.Claimant == tq.Claimant
			if !isExpired && !isOwned {
				continue
			}
		}

		t := f.toTask()
		if tq.OmitValues {
			t.Value = nil
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}
