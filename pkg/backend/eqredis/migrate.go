package eqredis

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

// moveDocKeyScript renames KEYS[1] to KEYS[2] when KEYS[1] holds a doc of
// namespace ARGV[1] and KEYS[2] is free. The namespace check matters: under
// the legacy encoding, a doc of the colliding namespace may occupy the key,
// and it already sits at its correct new key.
var moveDocKeyScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'namespace') ~= ARGV[1] then return 0 end
if redis.call('EXISTS', KEYS[2]) == 1 then return 0 end
redis.call('RENAME', KEYS[1], KEYS[2])
return 1
`)

// migrateDocKeys moves docs stored under legacyDocKey to docKey. Only
// namespaces containing "%" encode differently, so only their docs move. It
// is idempotent and runs on every Open, which also picks up docs written by
// an older server still sharing the database.
func migrateDocKeys(ctx context.Context, client *redis.Client) error {
	namespaces, err := client.SMembers(ctx, namespacesKey).Result()
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}
	for _, ns := range namespaces {
		if !strings.Contains(ns, "%") {
			continue
		}
		members, err := client.ZRange(ctx, docNSIndexKey(ns), 0, -1).Result()
		if err != nil {
			return fmt.Errorf("list docs in %q: %w", ns, err)
		}
		for _, member := range members {
			_, _, id := parseDocIndexMember(member)
			keys := []string{legacyDocKey(ns, id), docKey(ns, id)}
			if err := moveDocKeyScript.Run(ctx, client, keys, ns).Err(); err != nil {
				return fmt.Errorf("move doc %q in %q: %w", id, ns, err)
			}
		}
	}
	return nil
}

// claimsIndexMigratedKey marks a database whose queues all have a qsclaims
// index and whose qsclaimed sets hold only tasks that were claimed.
const claimsIndexMigratedKey = keyPrefix + "migrated:claimsindex"

// migrateClaimsIndex builds each queue's qsclaims index from its tasks' claim
// counts, and drops from qsclaimed the tasks that were never claimed, which
// an earlier version added when a change moved them into the future. It runs
// once per database.
func migrateClaimsIndex(ctx context.Context, client *redis.Client) error {
	done, err := client.Exists(ctx, claimsIndexMigratedKey).Result()
	if err != nil {
		return fmt.Errorf("check claims index migration: %w", err)
	}
	if done == 1 {
		return nil
	}
	queues, err := client.SMembers(ctx, queuesKey).Result()
	if err != nil {
		return fmt.Errorf("list queues: %w", err)
	}
	for _, q := range queues {
		ids, err := client.ZRange(ctx, queueKey(q), 0, -1).Result()
		if err != nil {
			return fmt.Errorf("list tasks in %q: %w", q, err)
		}
		read := client.Pipeline()
		claims := make([]*redis.StringCmd, len(ids))
		for i, id := range ids {
			claims[i] = read.HGet(ctx, taskKey(id), "claims")
		}
		if _, err := read.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("read claims in %q: %w", q, err)
		}
		write := client.Pipeline()
		for i, id := range ids {
			n, err := claims[i].Int()
			if err != nil || n == 0 {
				write.ZRem(ctx, qsclaimedKey(q), id)
				continue
			}
			write.ZAdd(ctx, qsclaimsKey(q), redis.Z{Score: float64(n), Member: id})
		}
		if _, err := write.Exec(ctx); err != nil {
			return fmt.Errorf("index claims in %q: %w", q, err)
		}
	}
	if err := client.Set(ctx, claimsIndexMigratedKey, "1", 0).Err(); err != nil {
		return fmt.Errorf("mark claims index migration: %w", err)
	}
	return nil
}
