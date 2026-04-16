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
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shiblon/entroq"
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

// docFields holds all fields stored in a doc Hash.
type docFields struct {
	Namespace    string
	ID           string
	Version      int32
	Claimant     string
	AtMs         int64
	KeyPrimary   string
	KeySecondary string
	Content      []byte
	Created      int64
	Modified     int64
}

func (f *docFields) toDoc() *entroq.Doc {
	var at time.Time
	if f.AtMs > 0 {
		at = time.UnixMilli(f.AtMs).UTC()
	}
	return &entroq.Doc{
		Namespace:    f.Namespace,
		ID:           f.ID,
		Version:      f.Version,
		Claimant:     f.Claimant,
		At:           at,
		Key:          f.KeyPrimary,
		SecondaryKey: f.KeySecondary,
		Content:      f.Content,
		Created:      time.UnixMilli(f.Created).UTC(),
		Modified:     time.UnixMilli(f.Modified).UTC(),
	}
}

func (f *docFields) toMap() map[string]interface{} {
	return map[string]interface{}{
		"namespace":     f.Namespace,
		"id":            f.ID,
		"version":       strconv.FormatInt(int64(f.Version), 10),
		"claimant":      f.Claimant,
		"at":            strconv.FormatInt(f.AtMs, 10),
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

	version, err := parseInt(vals["version"])
	if err != nil {
		return nil, fmt.Errorf("parse doc version: %w", err)
	}
	at, err := parseInt(vals["at"])
	if err != nil {
		return nil, fmt.Errorf("parse doc at: %w", err)
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
		Version:      int32(version),
		Claimant:     vals["claimant"],
		AtMs:         at,
		KeyPrimary:   vals["key_primary"],
		KeySecondary: vals["key_secondary"],
		Content:      content,
		Created:      created,
		Modified:     modified,
	}, nil
}

// Docs returns docs in a namespace, optionally filtered by key range or IDs.
func (e *EQRedis) Docs(ctx context.Context, rq *entroq.DocQuery) ([]*entroq.Doc, error) {
	var ids []string

	if len(rq.IDs) > 0 {
		ids = rq.IDs
	} else {
		// Use namespace index for key-range scan.
		idxKey := docNSIndexKey(rq.Namespace)
		min := "-"
		max := "+"
		if rq.KeyStart != "" {
			min = "[" + rq.KeyStart
		}
		if rq.KeyEnd != "" {
			// Half-open range [start, end): exclude members >= end.
			max = "(" + rq.KeyEnd
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
			_, _, id := parseDocIndexMember(m)
			ids = append(ids, id)
		}
	}

	if len(ids) == 0 {
		return nil, nil
	}

	// Pipeline HGETALL for each doc.
	pipe := e.client.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, docKey(rq.Namespace, id))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("docs pipeline hgetall: %w", err)
	}

	var docs []*entroq.Doc
	for i, cmd := range cmds {
		vals, err := cmd.Result()
		if errors.Is(err, redis.Nil) || len(vals) == 0 {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("docs hgetall %q: %w", ids[i], err)
		}
		f, err := parseDocFields(vals)
		if err != nil {
			return nil, fmt.Errorf("docs parse %q: %w", ids[i], err)
		}
		d := f.toDoc()
		if rq.OmitValues {
			d.Content = nil
		}
		docs = append(docs, d)
	}
	return docs, nil
}

// ClaimDocs claims all docs sharing a primary key in a namespace.
// Returns a DependencyError if any doc with that key is already claimed
// by a different claimant.
func (e *EQRedis) ClaimDocs(ctx context.Context, cq *entroq.DocClaim) ([]*entroq.Doc, error) {
	now := time.Now().UTC()
	nowMs := now.UnixMilli()
	newAtMs := now.Add(cq.Duration).UnixMilli()
	idxKey := docNSIndexKey(cq.Namespace)

	for range maxClaimRetries {
		// Find all doc IDs with this primary key using the namespace index.
		min := "[" + cq.Key + docIndexSep
		// The key immediately after all entries with this prefix:
		// increment the last byte of Key to get the exclusive upper bound.
		maxKey := cq.Key + "\x01"
		max := "(" + maxKey

		members, err := e.client.ZRangeArgs(ctx, redis.ZRangeArgs{
			Key:   idxKey,
			Start: min,
			Stop:  max,
			ByLex: true,
		}).Result()
		if err != nil {
			return nil, fmt.Errorf("claim docs index scan: %w", err)
		}
		if len(members) == 0 {
			return nil, nil
		}

		var docIDs []string
		for _, m := range members {
			_, _, id := parseDocIndexMember(m)
			docIDs = append(docIDs, id)
		}

		watchKeys := make([]string, len(docIDs))
		for i, id := range docIDs {
			watchKeys[i] = docKey(cq.Namespace, id)
		}

		var claimed []*entroq.Doc

		err = e.client.Watch(ctx, func(tx *redis.Tx) error {
			// Read current state of all docs.
			pipe := tx.Pipeline()
			cmds := make([]*redis.MapStringStringCmd, len(docIDs))
			for i, id := range docIDs {
				cmds[i] = pipe.HGetAll(ctx, docKey(cq.Namespace, id))
			}
			if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
				return fmt.Errorf("claim docs read: %w", err)
			}

			var fields []*docFields
			for _, cmd := range cmds {
				vals, err := cmd.Result()
				if errors.Is(err, redis.Nil) || len(vals) == 0 {
					continue
				}
				if err != nil {
					return fmt.Errorf("claim docs hgetall: %w", err)
				}
				f, err := parseDocFields(vals)
				if err != nil {
					return fmt.Errorf("claim docs parse: %w", err)
				}
				fields = append(fields, f)
			}

			// Check for docs already claimed by a different claimant.
			depErr := &entroq.DependencyError{}
			for _, f := range fields {
				alreadyClaimed := f.Claimant != "" && f.Claimant != cq.Claimant && f.AtMs > nowMs
				if alreadyClaimed {
					depErr.DocClaims = append(depErr.DocClaims, &entroq.DocID{
						Namespace: f.Namespace,
						ID:        f.ID,
						Version:   f.Version,
					})
				}
			}
			if depErr.HasAny() {
				return depErr
			}

			// Atomically claim all docs.
			claimed = nil
			_, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				for _, f := range fields {
					updated := &docFields{
						Namespace:    f.Namespace,
						ID:           f.ID,
						Version:      f.Version + 1,
						Claimant:     cq.Claimant,
						AtMs:         newAtMs,
						KeyPrimary:   f.KeyPrimary,
						KeySecondary: f.KeySecondary,
						Content:      f.Content,
						Created:      f.Created,
						Modified:     nowMs,
					}
					pipe.HSet(ctx, docKey(f.Namespace, f.ID), updated.toMap())
					claimed = append(claimed, updated.toDoc())
				}
				return nil
			})
			return err
		}, watchKeys...)

		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		if depErr, ok := err.(*entroq.DependencyError); ok {
			return nil, depErr
		}
		if err != nil {
			return nil, fmt.Errorf("claim docs watch: %w", err)
		}
		return claimed, nil
	}
	return nil, fmt.Errorf("claimdocs max retries (%d) reached", maxClaimRetries)
}
