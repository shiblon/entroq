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

	// A doc is claimed while its group is held: count the members of each
	// namespace's held groups.
	pipe := e.client.Pipeline()
	zcardCmds := make(map[string]*redis.IntCmd, len(names))
	heldCmds := make(map[string]*redis.StringSliceCmd, len(names))
	for _, ns := range names {
		zcardCmds[ns] = pipe.ZCard(ctx, docNSIndexKey(ns))
		heldCmds[ns] = pipe.ZRangeArgs(ctx, redis.ZRangeArgs{
			Key: heldGroupsKey(ns), Start: fmt.Sprintf("(%d", now), Stop: "+inf", ByScore: true,
		})
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("namespace stats pipeline: %w", err)
	}

	pipe = e.client.Pipeline()
	claimedCmds := make(map[string][]*redis.IntCmd, len(names))
	for _, ns := range names {
		for _, key := range heldCmds[ns].Val() {
			min, max := groupMembersRange(key)
			claimedCmds[ns] = append(claimedCmds[ns], pipe.ZLexCount(ctx, docNSIndexKey(ns), min, max))
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("namespace stats claimed pipeline: %w", err)
	}

	result := make(map[string]*entroq.NamespaceStat, len(names))
	for _, ns := range names {
		size := int(zcardCmds[ns].Val())
		if size == 0 {
			continue
		}
		claimed := 0
		for _, cmd := range claimedCmds[ns] {
			claimed += int(cmd.Val())
		}
		result[ns] = &entroq.NamespaceStat{
			Name:    ns,
			Size:    size,
			Claimed: claimed,
		}
	}
	return result, nil
}
