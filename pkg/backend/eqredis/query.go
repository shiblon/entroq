package eqredis

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shiblon/entroq"
)

// matchesQueuesQuery returns true if the queue name passes the filter in qq.
func matchesQueuesQuery(name string, qq *entroq.QueuesQuery) bool {
	if len(qq.MatchExact) > 0 {
		return slices.Contains(qq.MatchExact, name)
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
//	Size      = ZCARD eq:q:{name}
//	Claimed   = SCARD eq:inflight:{name}
//	Available = ZCOUNT eq:q:{name} 0 now_ms
//	            (claimed tasks have score > now by definition, so this is exact)
//	Future    = Size - Available - Claimed
//	            (approximation: includes expired-but-rescheduled tasks with claims>0)
//	MaxClaims = 0 (always; see TestQueueStats in redis_test.go)
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

	// Pipeline ZCARD, ZCOUNT (available), and ZCOUNT >now (claimed) for all queues.
	pipe := e.client.Pipeline()
	zcardCmds := make(map[string]*redis.IntCmd, len(names))
	zcountCmds := make(map[string]*redis.IntCmd, len(names))
	claimedCmds := make(map[string]*redis.IntCmd, len(names))
	nowStr := fmt.Sprintf("%d", nowMs)

	for _, name := range names {
		zcardCmds[name] = pipe.ZCard(ctx, queueKey(name))
		zcountCmds[name] = pipe.ZCount(ctx, queueKey(name), "0", nowStr)
		claimedCmds[name] = pipe.ZCount(ctx, qsclaimedKey(name), fmt.Sprintf("(%d", nowMs), "+inf")
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
		claimed := int(claimedCmds[name].Val())
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

// claimListPage is the floor page size when scanning the future-dated region of
// a queue for a specific claimant's tasks. Pages grow geometrically from here.
const claimListPage = 128

// Tasks retrieves tasks from a queue, optionally filtered by claimant or IDs.
//
// Limit caps the number of MATCHING tasks returned -- min(Limit, #matching) --
// not the number of candidates inspected. That distinction drives the three
// paths below. A claimant filter keeps tasks that are available/expired
// (arrival time <= now) OR claimed by that claimant; applying Limit before the
// filter could return fewer than Limit, even zero, while matches remain, so we
// never do that.
func (e *EQRedis) Tasks(ctx context.Context, tq *entroq.TasksQuery) ([]*entroq.Task, error) {
	now := time.Now().UTC()

	// Explicit IDs: fetch exactly those hashes, filter, then cap results.
	if len(tq.IDs) > 0 {
		var keep func(*taskFields) bool
		if tq.Claimant != "" {
			keep = claimantKeep(tq.Claimant, now)
		}
		tasks, err := e.hydrate(ctx, tq.IDs, tq, keep)
		if err != nil {
			return nil, err
		}
		if tq.Limit > 0 && len(tasks) > tq.Limit {
			tasks = tasks[:tq.Limit]
		}
		return tasks, nil
	}

	if tq.Queue == "" {
		// No queue and no IDs: not supported efficiently without a full keyspace scan.
		return nil, fmt.Errorf("tasks: queue or IDs must be specified")
	}

	// No claimant filter: any Limit tasks. Push the limit into the range read so
	// a large queue does not force pulling every member id out of Redis.
	if tq.Claimant == "" {
		stop := int64(-1)
		if tq.Limit > 0 {
			stop = int64(tq.Limit - 1)
		}
		ids, err := e.client.ZRange(ctx, queueKey(tq.Queue), 0, stop).Result()
		if err != nil {
			return nil, fmt.Errorf("tasks zrange %q: %w", tq.Queue, err)
		}
		return e.hydrate(ctx, ids, tq, nil)
	}

	return e.tasksForClaimant(ctx, tq, now)
}

// claimantKeep returns the claimant filter predicate: keep tasks that are
// available/expired (arrival time at or before now) or claimed by claimant.
func claimantKeep(claimant string, now time.Time) func(*taskFields) bool {
	return func(f *taskFields) bool {
		expired := !now.Before(time.UnixMilli(f.AtMs).UTC())
		return expired || f.Claimant == claimant
	}
}

// tasksForClaimant lists a queue filtered by claimant. It exploits the fact that
// the "available/expired" half of the filter is exactly "arrival time <= now",
// which is the ZSET score: those ids come straight out of a score range with no
// hash reads. Only the future-dated region (score > now) needs hashes read, and
// only to check the claimant, and only when the available tasks alone did not
// fill Limit.
func (e *EQRedis) tasksForClaimant(ctx context.Context, tq *entroq.TasksQuery, now time.Time) ([]*entroq.Task, error) {
	qkey := queueKey(tq.Queue)
	limit := tq.Limit // 0 means unlimited
	nowStr := strconv.FormatInt(now.UnixMilli(), 10)

	// Region A: available/expired tasks. All satisfy the filter, so no hash is
	// read to decide -- hydrate them directly.
	availArgs := &redis.ZRangeBy{Min: "0", Max: nowStr}
	if limit > 0 {
		availArgs.Count = int64(limit)
	}
	availIDs, err := e.client.ZRangeByScore(ctx, qkey, availArgs).Result()
	if err != nil {
		return nil, fmt.Errorf("tasks avail zrangebyscore %q: %w", tq.Queue, err)
	}
	tasks, err := e.hydrate(ctx, availIDs, tq, nil)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(tasks) >= limit {
		return tasks[:limit], nil
	}

	// Region B: future-dated tasks match only if claimed by tq.Claimant, which
	// requires reading the hash. Page through with a geometric taper, stopping
	// when Limit is filled or the region is exhausted. No cap: honoring
	// min(Limit, #matching) means scanning to exhaustion when we come up short.
	keep := func(f *taskFields) bool { return f.Claimant == tq.Claimant }
	minB := "(" + nowStr
	page := int64(claimListPage)
	if limit > 0 {
		if rem := int64(limit - len(tasks)); rem > page {
			page = rem
		}
	}
	var offset int64
	for {
		ids, err := e.client.ZRangeByScore(ctx, qkey, &redis.ZRangeBy{
			Min: minB, Max: "+inf", Offset: offset, Count: page,
		}).Result()
		if err != nil {
			return nil, fmt.Errorf("tasks claimed zrangebyscore %q: %w", tq.Queue, err)
		}
		if len(ids) == 0 {
			break
		}
		matched, err := e.hydrate(ctx, ids, tq, keep)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, matched...)
		if limit > 0 && len(tasks) >= limit {
			return tasks[:limit], nil
		}
		offset += int64(len(ids))
		if int64(len(ids)) < page {
			break // short page: region exhausted
		}
		page *= 2 // geometric taper: fewer round-trips as we scan deeper
	}
	return tasks, nil
}

// hydrate pipelines HGETALL over ids in order, parses each, drops any deleted
// concurrently, applies OmitValues, and (when keep is non-nil) keeps only tasks
// for which keep reports true. Input order is preserved.
func (e *EQRedis) hydrate(ctx context.Context, ids []string, tq *entroq.TasksQuery, keep func(*taskFields) bool) ([]*entroq.Task, error) {
	if len(ids) == 0 {
		return nil, nil
	}

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
			// Task was deleted between the range read and now; skip.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("tasks hgetall %q: %w", ids[i], err)
		}

		f, err := parseTaskFields(vals)
		if err != nil {
			return nil, fmt.Errorf("tasks parse %q: %w", ids[i], err)
		}
		if keep != nil && !keep(f) {
			continue
		}

		t := f.toTask()
		if tq.OmitValues {
			t.Value = nil
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}
