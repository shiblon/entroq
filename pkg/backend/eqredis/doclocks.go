package eqredis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/docgroup"
)

// lockKey is the hash holding a doc group's lock: the only version, claimant,
// and arrival time its members have. A group can be claimed before it has
// docs, so the lock is kept apart from them. The key is last, after the
// escaped namespace, as in docKey.
func lockKey(g docgroup.Group) string {
	return keyPrefix + "dl:" + docKeyEscaper.Replace(g.Namespace) + "/" + g.Key
}

// lockIndexKey is the set of primary keys in a namespace that have a lock.
func lockIndexKey(ns string) string {
	return keyPrefix + "dlidx:" + ns
}

// heldGroupsKey is the sorted set of primary keys in a namespace whose group
// is claimed, scored by when the claim ends.
func heldGroupsKey(ns string) string {
	return keyPrefix + "dlheld:" + ns
}

// docLocksMigratedKey marks a database whose doc groups all have locks.
const docLocksMigratedKey = keyPrefix + "migrated:doclocks"

// groupMembersRange bounds a group's entries in the namespace doc index, which
// continue with docIndexSep after the primary key.
func groupMembersRange(key string) (min, max string) {
	return "[" + key + docIndexSep, "(" + key + "\x01"
}

func parseLock(vals map[string]string) (docgroup.Lock, error) {
	if len(vals) == 0 {
		return docgroup.Absent, nil
	}
	version, err := strconv.ParseInt(vals["version"], 10, 32)
	if err != nil {
		return docgroup.Lock{}, fmt.Errorf("parse doc lock version: %w", err)
	}
	at, err := strconv.ParseInt(vals["at"], 10, 64)
	if err != nil {
		return docgroup.Lock{}, fmt.Errorf("parse doc lock at: %w", err)
	}
	return docgroup.Lock{Version: int32(version), Claimant: vals["claimant"], At: time.UnixMilli(at).UTC()}, nil
}

// readLocks reads the locks of groups; a group with none maps to
// docgroup.Absent.
func readLocks(ctx context.Context, c redis.Cmdable, groups []docgroup.Group) (map[docgroup.Group]docgroup.Lock, error) {
	locks := make(map[docgroup.Group]docgroup.Lock, len(groups))
	if len(groups) == 0 {
		return locks, nil
	}
	pipe := c.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(groups))
	for i, g := range groups {
		cmds[i] = pipe.HGetAll(ctx, lockKey(g))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("read doc locks: %w", err)
	}
	for i, g := range groups {
		l, err := parseLock(cmds[i].Val())
		if err != nil {
			return nil, err
		}
		locks[g] = l
	}
	return locks, nil
}

// writeLock queues writing g's lock and its bookkeeping in pipe.
func writeLock(ctx context.Context, pipe redis.Pipeliner, g docgroup.Group, l docgroup.Lock, now time.Time) {
	pipe.HSet(ctx, lockKey(g), map[string]any{
		"version":  strconv.FormatInt(int64(l.Version), 10),
		"claimant": l.Claimant,
		"at":       strconv.FormatInt(l.At.UnixMilli(), 10),
	})
	pipe.SAdd(ctx, lockIndexKey(g.Namespace), g.Key)
	pipe.SAdd(ctx, namespacesKey, g.Namespace)
	if l.Held(now) {
		pipe.ZAdd(ctx, heldGroupsKey(g.Namespace), redis.Z{Score: float64(l.At.UnixMilli()), Member: g.Key})
	} else {
		pipe.ZRem(ctx, heldGroupsKey(g.Namespace), g.Key)
	}
}

// groupMembers reads the docs of the group g.
func groupMembers(ctx context.Context, c redis.Cmdable, g docgroup.Group) ([]*entroq.Doc, error) {
	min, max := groupMembersRange(g.Key)
	members, err := c.ZRangeArgs(ctx, redis.ZRangeArgs{Key: docNSIndexKey(g.Namespace), Start: min, Stop: max, ByLex: true}).Result()
	if err != nil {
		return nil, fmt.Errorf("list doc group members: %w", err)
	}
	if len(members) == 0 {
		return nil, nil
	}
	pipe := c.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(members))
	for i, m := range members {
		_, _, id := parseDocIndexMember(m)
		cmds[i] = pipe.HGetAll(ctx, docKey(g.Namespace, id))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("read doc group members: %w", err)
	}
	var docs []*entroq.Doc
	for _, cmd := range cmds {
		vals := cmd.Val()
		if len(vals) == 0 {
			continue
		}
		f, err := parseDocFields(vals)
		if err != nil {
			return nil, err
		}
		docs = append(docs, f.toDoc())
	}
	return docs, nil
}

// migrateDocLocks gives every doc group a lock, once per database. Before doc
// groups had locks each doc carried its own version and claim; a group's lock
// starts one version past its highest member, so any version read before the
// migration is stale. Claims held at migration time are released, and the old
// per-doc claim sets are removed.
func migrateDocLocks(ctx context.Context, client *redis.Client) error {
	done, err := client.Exists(ctx, docLocksMigratedKey).Result()
	if err != nil {
		return fmt.Errorf("check doc lock migration: %w", err)
	}
	if done == 1 {
		return nil
	}
	namespaces, err := client.SMembers(ctx, namespacesKey).Result()
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}
	now := time.Now().UTC()
	for _, ns := range namespaces {
		members, err := client.ZRange(ctx, docNSIndexKey(ns), 0, -1).Result()
		if err != nil {
			return fmt.Errorf("list docs in %q: %w", ns, err)
		}
		highest := make(map[string]int32)
		for _, m := range members {
			key, _, id := parseDocIndexMember(m)
			v, err := client.HGet(ctx, docKey(ns, id), "version").Int()
			if errors.Is(err, redis.Nil) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read version of doc %q in %q: %w", id, ns, err)
			}
			if cur, ok := highest[key]; !ok || int32(v) > cur {
				highest[key] = int32(v)
			}
		}
		pipe := client.TxPipeline()
		for key, v := range highest {
			g := docgroup.Group{Namespace: ns, Key: key}
			if n, err := client.Exists(ctx, lockKey(g)).Result(); err != nil {
				return fmt.Errorf("check lock of %q in %q: %w", key, ns, err)
			} else if n == 1 {
				continue
			}
			writeLock(ctx, pipe, g, docgroup.Lock{Version: v + 1, At: now}, now)
		}
		pipe.Del(ctx, nsclaimedKey(ns))
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("create doc locks in %q: %w", ns, err)
		}
	}
	if err := client.Set(ctx, docLocksMigratedKey, "1", 0).Err(); err != nil {
		return fmt.Errorf("mark doc lock migration: %w", err)
	}
	return nil
}

// collectLockScript deletes a doc group's lock if the group is unheld and has
// no docs. Inserts into an unheld group leave its lock alone, so a WATCH on the
// lock could not see one land between checking for docs and deleting; the
// script does both atomically. An insert already watching the lock fails its
// watch when the lock goes, and retries against a new group.
//
//	KEYS[1]=lock hash  KEYS[2]=namespace doc index  KEYS[3]=lock index set
//	KEYS[4]=held groups ZSET
//	ARGV[1]=primary key  ARGV[2]=member range min  ARGV[3]=member range max
//	ARGV[4]=nowMs
//	returns 1 if the lock was deleted, 0 otherwise.
var collectLockScript = redis.NewScript(`
local l = redis.call('HMGET', KEYS[1], 'claimant', 'at')
if l[1] and l[1] ~= '' and tonumber(l[2]) > tonumber(ARGV[4]) then return 0 end
if redis.call('ZLEXCOUNT', KEYS[2], ARGV[2], ARGV[3]) > 0 then return 0 end
redis.call('SREM', KEYS[3], ARGV[1])
redis.call('ZREM', KEYS[4], ARGV[1])
return redis.call('DEL', KEYS[1])
`)

// collectLocksOnce removes the locks of up to batch groups that have no docs
// and are not held, so claimed-then-abandoned groups do not accumulate.
func (e *EQRedis) collectLocksOnce(ctx context.Context, batch int) (int, error) {
	namespaces, err := e.client.SMembers(ctx, namespacesKey).Result()
	if err != nil {
		return 0, fmt.Errorf("list namespaces: %w", err)
	}
	collected := 0
	for _, ns := range namespaces {
		keys, err := e.client.SMembers(ctx, lockIndexKey(ns)).Result()
		if err != nil {
			return collected, fmt.Errorf("list doc locks in %q: %w", ns, err)
		}
		for _, key := range keys {
			if collected >= batch || ctx.Err() != nil {
				return collected, ctx.Err()
			}
			g := docgroup.Group{Namespace: ns, Key: key}
			min, max := groupMembersRange(key)
			n, err := collectLockScript.Run(ctx, e.client,
				[]string{lockKey(g), docNSIndexKey(ns), lockIndexKey(ns), heldGroupsKey(ns)},
				key, min, max, time.Now().UnixMilli()).Int()
			if err != nil {
				return collected, fmt.Errorf("collect lock of %q in %q: %w", key, ns, err)
			}
			collected += n
		}
	}
	return collected, nil
}
