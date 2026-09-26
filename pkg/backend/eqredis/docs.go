package eqredis

// Doc storage for eqredis.
//
// Key layout:
//   eq:d:{ns}/{id}    -- Hash: all doc fields
//   eq:dnsidx:{ns}    -- ZSET: score=0, member="{key_primary}\x00{key_secondary}\x00{id}"
//                        Enables ZRANGEBYLEX range scans on key_primary.
//
// The namespace index is maintained atomically inside Modify's MULTI block.
// ClaimDocs uses WATCH + MULTI/EXEC on all doc hashes sharing the primary key.

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
	"github.com/shiblon/entroq/pkg/backend/internal/docgroup"
	"github.com/shiblon/entroq/pkg/backend/internal/validate"
)

// docIndexSep is the separator used between fields in a namespace index member.
// A null byte has ideal lexicographic properties: it sorts before all printable
// characters, so range scans on key_primary work correctly regardless of key content.
// It must not appear in key_primary or key_secondary values (validated on write).
const docIndexSep = "\x00"

func docNSIndexKey(namespace string) string {
	return keyPrefix + "dnsidx:" + namespace
}

// validateDocKeys returns an error if key_primary or key_secondary contain
// null bytes, which are used as separators in the namespace index members.
func validateDocKeys(keyPrimary, keySecondary string) error {
	if strings.ContainsRune(keyPrimary, 0) {
		return fmt.Errorf("doc key_primary must not contain null bytes")
	}
	if strings.ContainsRune(keySecondary, 0) {
		return fmt.Errorf("doc key_secondary must not contain null bytes")
	}
	return nil
}

// docIndexMember encodes a doc's index entry for ZRANGEBYLEX.
// Format: "{key_primary}{sep}{key_secondary}{sep}{id}"
// The id field is last and parsed with SplitN(..., 3), so null bytes in IDs
// are safe -- they are never interpreted as separators.
func docIndexMember(keyPrimary, keySecondary, id string) string {
	return keyPrimary + docIndexSep + keySecondary + docIndexSep + id
}

// parseDocIndexMember splits a ZRANGEBYLEX member back into its parts.
func parseDocIndexMember(member string) (keyPrimary, keySecondary, id string) {
	parts := strings.SplitN(member, docIndexSep, 3)
	if len(parts) == 3 {
		return parts[0], parts[1], parts[2]
	}
	return member, "", ""
}

// docFields holds all fields stored in a doc Hash. A doc's version, claimant,
// and arrival time are its group's, stored in the group's lock (lockKey).
type docFields struct {
	Namespace    string
	ID           string
	KeyPrimary   string
	KeySecondary string
	Content      []byte
	Created      int64
	Modified     int64
}

// toDoc returns the stored doc without its group's lock; see docgroup.Overlay.
func (f *docFields) toDoc() *entroq.Doc {
	return &entroq.Doc{
		Namespace:    f.Namespace,
		ID:           f.ID,
		Key:          f.KeyPrimary,
		SecondaryKey: f.KeySecondary,
		Content:      f.Content,
		Created:      time.UnixMilli(f.Created).UTC(),
		Modified:     time.UnixMilli(f.Modified).UTC(),
	}
}

func (f *docFields) toMap() map[string]any {
	return map[string]any{
		"namespace":     f.Namespace,
		"id":            f.ID,
		"key_primary":   f.KeyPrimary,
		"key_secondary": f.KeySecondary,
		"content":       string(f.Content),
		"created":       strconv.FormatInt(f.Created, 10),
		"modified":      strconv.FormatInt(f.Modified, 10),
	}
}

func parseDocFields(vals map[string]string) (*docFields, error) {
	parseInt := func(s string) (int64, error) {
		if s == "" {
			return 0, nil
		}
		return strconv.ParseInt(s, 10, 64)
	}

	created, err := parseInt(vals["created"])
	if err != nil {
		return nil, fmt.Errorf("parse doc created: %w", err)
	}
	modified, err := parseInt(vals["modified"])
	if err != nil {
		return nil, fmt.Errorf("parse doc modified: %w", err)
	}

	var content []byte
	if c := vals["content"]; c != "" {
		content = []byte(c)
	}

	return &docFields{
		Namespace:    vals["namespace"],
		ID:           vals["id"],
		KeyPrimary:   vals["key_primary"],
		KeySecondary: vals["key_secondary"],
		Content:      content,
		Created:      created,
		Modified:     modified,
	}, nil
}

// Docs returns docs in a namespace, optionally filtered by key range or IDs.
func (e *EQRedis) Docs(ctx context.Context, rq *entroq.DocQuery) ([]*entroq.Doc, error) {
	if err := rq.Validate(); err != nil {
		return nil, fmt.Errorf("eqredis docs: %w", err)
	}
	// ids are the docs to read, and keys their primary keys where known.
	var ids []string
	keys := make(map[string]string)

	if len(rq.IDs) > 0 {
		ids = rq.IDs
		// Look up the primary keys, which never change, so the docs and their
		// group locks can be read together below.
		pipe := e.client.Pipeline()
		cmds := make([]*redis.StringCmd, len(ids))
		for i, id := range ids {
			cmds[i] = pipe.HGet(ctx, docKey(rq.Namespace, id), "key_primary")
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return nil, fmt.Errorf("docs key lookup: %w", err)
		}
		for i, cmd := range cmds {
			if key, err := cmd.Result(); err == nil {
				keys[ids[i]] = key
			}
		}
	} else {
		// Use namespace index for key-range scan.
		idxKey := docNSIndexKey(rq.Namespace)
		min := "-"
		max := "+"
		if rq.KeyExact != "" {
			// Index members continue with NUL after the primary key. Bound
			// that complete prefix, excluding the next possible byte value.
			min = "[" + rq.KeyExact + docIndexSep
			max = "(" + rq.KeyExact + "\x01"
		} else {
			if rq.KeyStart != "" {
				min = "[" + rq.KeyStart
			}
			if rq.KeyEnd != "" {
				// Half-open range [start, end): exclude members >= end.
				max = "(" + rq.KeyEnd
			}
		}

		members, err := e.client.ZRangeArgs(ctx, redis.ZRangeArgs{
			Key:   idxKey,
			Start: min,
			Stop:  max,
			ByLex: true,
		}).Result()
		if err != nil {
			return nil, fmt.Errorf("docs zrangebylex %q: %w", rq.Namespace, err)
		}

		limit := rq.Limit
		if limit > 0 && len(members) > limit {
			members = members[:limit]
		}

		for _, m := range members {
			key, _, id := parseDocIndexMember(m)
			ids = append(ids, id)
			keys[id] = key
		}
	}

	if len(ids) == 0 {
		return nil, nil
	}
	for {
		docs, ok, err := e.readDocsWithLocks(ctx, rq, ids, keys)
		if err != nil || ok {
			return docs, err
		}
	}
}

// readDocsWithLocks reads docs and their groups' locks in one transaction, so
// each doc's content and its group's version come from the same moment. Read
// apart, a write landing between them would pair old content with the new
// version, and a read-modify-write of that content would pass its version
// check and overwrite the newer content. keys gives each doc's primary key,
// read beforehand; it returns false if a doc turned out to belong to another
// group, deleted and inserted again meanwhile, and the caller reads again.
func (e *EQRedis) readDocsWithLocks(ctx context.Context, rq *entroq.DocQuery, ids []string, keys map[string]string) ([]*entroq.Doc, bool, error) {
	var groups []docgroup.Group
	for _, id := range ids {
		if key, ok := keys[id]; ok {
			if g := (docgroup.Group{Namespace: rq.Namespace, Key: key}); !slices.Contains(groups, g) {
				groups = append(groups, g)
			}
		}
	}
	docCmds := make([]*redis.MapStringStringCmd, len(ids))
	lockCmds := make([]*redis.MapStringStringCmd, len(groups))
	if _, err := e.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, id := range ids {
			docCmds[i] = pipe.HGetAll(ctx, docKey(rq.Namespace, id))
		}
		for i, g := range groups {
			lockCmds[i] = pipe.HGetAll(ctx, lockKey(g))
		}
		return nil
	}); err != nil && !errors.Is(err, redis.Nil) {
		return nil, false, fmt.Errorf("eqredis docs: %w", err)
	}
	locks := make(map[docgroup.Group]docgroup.Lock, len(groups))
	for i, g := range groups {
		l, err := parseLock(lockCmds[i].Val())
		if err != nil {
			return nil, false, fmt.Errorf("eqredis docs: %w", err)
		}
		locks[g] = l
	}

	var docs []*entroq.Doc
	for i, cmd := range docCmds {
		vals := cmd.Val()
		if len(vals) == 0 {
			continue
		}
		f, err := parseDocFields(vals)
		if err != nil {
			return nil, false, fmt.Errorf("docs parse %q: %w", ids[i], err)
		}
		d := f.toDoc()
		if rq.OmitValues {
			d.Content = nil
		}
		l, ok := locks[docgroup.Group{Namespace: d.Namespace, Key: d.Key}]
		if !ok {
			return nil, false, nil
		}
		// Each doc carries its group's version and claim, the only ones it has.
		if l != docgroup.Absent {
			d = docgroup.Overlay(d, l)
		}
		docs = append(docs, d)
	}
	return docs, true, nil
}

// ClaimDocs claims the group of docs sharing a primary key in a namespace and
// returns its members, which may be none: a group can be claimed before it has
// docs. It returns a DependencyError listing the members while someone else
// holds the group.
//
// The claim is written before the members are read. Inserts into an unheld
// group leave its lock alone, so a claim watching the lock could not see one
// land between reading the members and claiming. Every insert watches the
// lock instead: one that commits before the claim is in the members read
// after it, and one that has not committed yet fails its watch when the claim
// writes the lock, then retries and finds the group held.
func (e *EQRedis) ClaimDocs(ctx context.Context, cq *entroq.DocClaim) ([]*entroq.Doc, error) {
	if err := validate.DocClaim(cq); err != nil {
		return nil, fmt.Errorf("eqredis claim docs: %w", err)
	}
	g := docgroup.Group{Namespace: cq.Namespace, Key: cq.Key}
	for attempt := range maxClaimRetries {
		var current, next docgroup.Lock
		var ok bool
		err := e.client.Watch(ctx, func(tx *redis.Tx) error {
			now := time.Now().UTC()
			locks, err := readLocks(ctx, tx, []docgroup.Group{g})
			if err != nil {
				return err
			}
			current = locks[g]
			if next, ok = docgroup.Claim(current, cq.Claimant, now, cq.Duration); !ok {
				return nil
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				writeLock(ctx, pipe, g, next, now)
				return nil
			})
			return err
		}, lockKey(g))
		if errors.Is(err, redis.TxFailedErr) {
			if err := waitToRetry(ctx, attempt); err != nil {
				return nil, fmt.Errorf("eqredis claim docs: %w", err)
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("eqredis claim docs: %w", err)
		}
		members, err := groupMembers(ctx, e.client, g)
		if err != nil {
			return nil, fmt.Errorf("eqredis claim docs: %w", err)
		}
		if !ok {
			depErr := &entroq.DependencyError{}
			for _, d := range members {
				depErr.DocClaims = append(depErr.DocClaims, entroq.NewDocID(d.Namespace, d.ID, current.Version))
			}
			return nil, depErr
		}
		for i, d := range members {
			members[i] = docgroup.Overlay(d, next)
		}
		return members, nil
	}
	return nil, fmt.Errorf("eqredis claim docs: too much contention on %q in %q", cq.Key, cq.Namespace)
}
