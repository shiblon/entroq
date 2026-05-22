package eqredis

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shiblon/entroq"
)

// NamespaceStats returns doc counts per namespace, optionally filtered and limited.
func (e *EQRedis) NamespaceStats(ctx context.Context, qq *entroq.MatchQuery) (map[string]*entroq.NamespaceStat, error) {
	now := time.Now().UTC().UnixMilli()

	allNS, err := e.client.SMembers(ctx, namespacesKey).Result()
	if err != nil {
		return nil, fmt.Errorf("namespace stats smembers: %w", err)
	}

	var names []string
	for _, ns := range allNS {
		if matchesQueuesQuery(ns, qq) {
			names = append(names, ns)
		}
	}
	if qq.Limit > 0 && len(names) > qq.Limit {
		names = names[:qq.Limit]
	}
	if len(names) == 0 {
		return map[string]*entroq.NamespaceStat{}, nil
	}

	pipe := e.client.Pipeline()
	zcardCmds := make(map[string]*redis.IntCmd, len(names))
	claimedCmds := make(map[string]*redis.IntCmd, len(names))
	for _, ns := range names {
		zcardCmds[ns] = pipe.ZCard(ctx, docNSIndexKey(ns))
		claimedCmds[ns] = pipe.ZCount(ctx, nsclaimedKey(ns), fmt.Sprintf("(%d", now), "+inf")
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("namespace stats pipeline: %w", err)
	}

	result := make(map[string]*entroq.NamespaceStat, len(names))
	for _, ns := range names {
		size := int(zcardCmds[ns].Val())
		if size == 0 {
			continue
		}
		result[ns] = &entroq.NamespaceStat{
			Name:    ns,
			Size:    size,
			Claimed: int(claimedCmds[ns].Val()),
		}
	}
	return result, nil
}
