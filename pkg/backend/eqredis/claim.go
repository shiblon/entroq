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

// claimScript atomically claims one available task from a queue's ZSET.
//
// Lua runs atomically on Redis's single thread, so concurrent claimers are
// serialized: each claim pushes its task's at into the future before the next
// script runs. That removes the WATCH/MULTI optimistic-retry loop the Go version
// needed AND the random pick's *collision-spreading* purpose.
//
// It does NOT remove the random pick's OTHER purpose: anti-starvation. Random
// selection among the most-overdue available tasks is part of EntroQ's contract
// (progress guarantee: a persistently-failing task must not deterministically
// re-claim and starve the rest of the queue). See eqmem's claimHeap.RandomAvailable
// for the sibling implementation. So we keep the claimWindow of lowest-at
// candidates and pick uniformly among them, seeded by a random offset passed in
// from Go (Redis's own Lua math.random is deterministically seeded, so it can't
// provide real randomness; passing the offset also keeps the script deterministic
// given ARGV, hence replication-safe). Scanning forward from the offset also
// skips any stale ZSET member whose hash was already removed, cleaning it up.
//
//	KEYS[1]=queue ZSET  KEYS[2]=qsclaimed ZSET  KEYS[3]=queues set
//	ARGV[1]=nowMs  ARGV[2]=newAtMs  ARGV[3]=claimant  ARGV[4]=window
//	ARGV[5]=queue name  ARGV[6]=task-key prefix  ARGV[7]=random offset
//	returns HGETALL of the claimed task, or nil if none claimable.
var claimScript = redis.NewScript(`
local ids = redis.call('ZRANGEBYSCORE', KEYS[1], '0', ARGV[1], 'LIMIT', 0, tonumber(ARGV[4]))
local n = #ids
if n == 0 then return false end
local start = tonumber(ARGV[7]) % n
for off = 0, n - 1 do
  local id = ids[(start + off) % n + 1]
  local tkey = ARGV[6] .. id
  local vals = redis.call('HGETALL', tkey)
  if #vals == 0 then
    redis.call('ZREM', KEYS[1], id)
  else
    local h = {}
    for i = 1, #vals, 2 do h[vals[i]] = vals[i+1] end
    redis.call('HSET', tkey,
      'claimant', ARGV[3],
      'at', ARGV[2],
      'version', tostring((tonumber(h['version']) or 0) + 1),
      'claims', tostring((tonumber(h['claims']) or 0) + 1),
      'modified', ARGV[1])
    redis.call('ZADD', KEYS[1], ARGV[2], id)
    redis.call('ZADD', KEYS[2], ARGV[2], id)
    redis.call('SADD', KEYS[3], ARGV[5])
    return redis.call('HGETALL', tkey)
  end
end
return false
`)

// tryClaimOne atomically claims a single task from the given queue via
// claimScript. Returns nil, nil if no task is available.
func (e *EQRedis) tryClaimOne(ctx context.Context, queue string, claimant string, duration time.Duration, now time.Time) (*entroq.Task, error) {
	nowMs := now.UnixMilli()
	newAtMs := now.Add(duration).UnixMilli()
	// Random offset into the candidate window for anti-starvation (Redis Lua
	// can't randomize; 1<<30 stays exact as a Lua double for the modulo).
	offset := rand.Intn(1 << 30)

	res, err := claimScript.Run(ctx, e.client,
		[]string{queueKey(queue), qsclaimedKey(queue), queuesKey},
		nowMs, newAtMs, claimant, claimWindow, queue, keyPrefix+"t:", offset).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil // nothing claimable
	}
	if err != nil {
		return nil, fmt.Errorf("claim script %q: %w", queue, err)
	}

	arr, ok := res.([]interface{})
	if !ok {
		return nil, fmt.Errorf("claim script %q: unexpected result type %T", queue, res)
	}
	vals := make(map[string]string, len(arr)/2)
	for i := 0; i+1 < len(arr); i += 2 {
		k, _ := arr[i].(string)
		v, _ := arr[i+1].(string)
		vals[k] = v
	}
	f, err := parseTaskFields(vals)
	if err != nil {
		return nil, fmt.Errorf("claim script parse %q: %w", queue, err)
	}
	return f.toTask(), nil
}

// Claim blocks until a task is claimed or ctx is canceled.
func (e *EQRedis) Claim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	return entroq.WaitTryClaim(ctx, cq, e.TryClaim, e.nw)
}
