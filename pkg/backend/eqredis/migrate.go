package eqredis

import (
	"context"
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
