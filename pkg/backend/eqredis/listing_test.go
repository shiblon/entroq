package eqredis

// Listing performance analysis: where does Tasks() spend its time on a large
// queue, and does pushing the Limit down into ZRANGE (instead of fetching every
// member and trimming client-side) help?
//
// Opt-in (seeds a large queue, runs several seconds):
//   ENTROQ_REDIS_LISTING=1 go test ./pkg/backend/eqredis/ \
//       -run TestRedisListingLimit -v -timeout 10m
//
// Reuses the redis:7-alpine container from TestMain (redisAddr).

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shiblon/entroq"
)

const (
	lQueue = "listing/big"
	lTasks = 30000
	lLimit = 100
	lIters = 200
)

// timeit runs fn iters times and returns the latency distribution summary line.
func timeit(name string, iters int, fn func() error) (string, error) {
	lats := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		start := time.Now()
		if err := fn(); err != nil {
			return "", fmt.Errorf("%s iter %d: %w", name, i, err)
		}
		lats = append(lats, time.Since(start))
	}
	return summarize(name, lats), nil
}

func TestRedisListingLimit(t *testing.T) {
	if os.Getenv("ENTROQ_REDIS_LISTING") == "" {
		t.Skip("set ENTROQ_REDIS_LISTING=1 to run the listing perf analysis")
	}
	ctx := context.Background()

	b, err := Open(ctx, WithAddr(redisAddr), withGCInterval(time.Hour))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer b.Close()

	t.Logf("seeding %d tasks into %q ...", lTasks, lQueue)
	seedContention(ctx, t, b, lQueue, lTasks)

	qkey := queueKey(lQueue)

	// Decompose the cost: the two Redis round-trips Tasks() makes today.
	line, err := timeit("ZRANGE 0 -1 (all ids)", lIters, func() error {
		return b.client.ZRange(ctx, qkey, 0, -1).Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(line)

	line, err = timeit(fmt.Sprintf("ZRANGE 0 %d (limit ids)", lLimit-1), lIters, func() error {
		return b.client.ZRange(ctx, qkey, 0, int64(lLimit-1)).Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(line)

	// HGETALL cost for lLimit hashes (the bounded part, same in both designs).
	ids, err := b.client.ZRange(ctx, qkey, 0, int64(lLimit-1)).Result()
	if err != nil {
		t.Fatal(err)
	}
	line, err = timeit(fmt.Sprintf("pipeline HGETALL x%d", len(ids)), lIters, func() error {
		pipe := b.client.Pipeline()
		for _, id := range ids {
			pipe.HGetAll(ctx, taskKey(id))
		}
		_, err := pipe.Exec(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(line)

	t.Log("--- end-to-end Tasks() ---")

	// Current implementation: unbounded ZRANGE, trim, then HGETALL.
	line, err = timeit(fmt.Sprintf("Tasks(Limit=%d) CURRENT", lLimit), lIters, func() error {
		_, err := b.Tasks(ctx, &entroq.TasksQuery{Queue: lQueue, Limit: lLimit})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(line)

	// Prototype: push the limit into ZRANGE, then the same HGETALL + parse. This
	// is the no-filter fast path (correct only when Claimant is empty).
	line, err = timeit(fmt.Sprintf("Tasks(Limit=%d) PUSHDOWN proto", lLimit), lIters, func() error {
		pids, err := b.client.ZRange(ctx, qkey, 0, int64(lLimit-1)).Result()
		if err != nil {
			return err
		}
		pipe := b.client.Pipeline()
		cmds := make([]*redis.MapStringStringCmd, len(pids))
		for i, id := range pids {
			cmds[i] = pipe.HGetAll(ctx, taskKey(id))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return err
		}
		for _, cmd := range cmds {
			vals, err := cmd.Result()
			if err != nil || len(vals) == 0 {
				continue
			}
			if _, err := parseTaskFields(vals); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(line)

	// Full listing (no limit) for reference: what an unbounded Tasks() costs.
	line, err = timeit("Tasks(no limit) full scan", lIters/4, func() error {
		_, err := b.Tasks(ctx, &entroq.TasksQuery{Queue: lQueue})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(line)

	t.Log("--- claimant filter: region-B (future-dated) scan ---")
	// Build a queue with NO available tasks: a wall of tasks claimed by other
	// workers, plus a handful claimed by "me". This forces Tasks(Claimant=me)
	// past the cheap available region into the HGETALL-per-candidate region B,
	// exercising the geometric-taper paging. Two layouts bracket the taper:
	// "me" near the front (nearer expiry -> filled in the first page or two) and
	// "me" at the tail (farther expiry -> scan the whole wall).
	const (
		bWall = 4000
		bMine = 100
	)
	claimN := func(t *testing.T, queue, claimant string, n int, dur time.Duration) {
		for i := 0; i < n; i++ {
			task, err := b.TryClaim(ctx, &entroq.ClaimQuery{Queues: []string{queue}, Claimant: claimant, Duration: dur})
			if err != nil {
				t.Fatalf("claim %s: %v", claimant, err)
			}
			if task == nil {
				t.Fatalf("claim %s: queue drained early at %d", claimant, i)
			}
		}
	}

	for _, sc := range []struct {
		name             string
		mineDur, wallDur time.Duration
	}{
		{"me at front of region B", 5 * time.Minute, 10 * time.Minute},
		{"me at tail of region B", 10 * time.Minute, 5 * time.Minute},
	} {
		q := "listing/claimed/" + sc.name
		seedContention(ctx, t, b, q, bWall+bMine)
		claimN(t, q, "me", bMine, sc.mineDur)
		claimN(t, q, "other", bWall, sc.wallDur)

		line, err = timeit(fmt.Sprintf("Tasks(me,Limit=%d) %s", bMine, sc.name), 50, func() error {
			out, err := b.Tasks(ctx, &entroq.TasksQuery{Queue: q, Claimant: "me", Limit: bMine})
			if err != nil {
				return err
			}
			if len(out) != bMine {
				return fmt.Errorf("want %d matches, got %d", bMine, len(out))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Log(line)
	}
}
