package eqredis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/docgroup"
	"github.com/shiblon/entroq/pkg/backend/internal/validate"
)

// Modify atomically applies insertions, changes, deletions, and dependency
// checks to the task store.
//
// Algorithm:
//  1. WATCH all task keys for deletes, changes, and depends.
//  2. HGETALL each to read current state and versions.
//  3. Verify all versions match caller expectations. On mismatch, return
//     a DependencyError immediately (semantic failure, no retry).
//  4. MULTI ... all mutations ... EXEC.
//  5. If EXEC returns TxFailedErr (WATCH fired), retry from step 1.
//
// On success, changed tasks have their claimant field cleared: a task that
// has been successfully modified is by definition no longer claimed.
func (e *EQRedis) Modify(ctx context.Context, mod *entroq.Modification) (*entroq.ModifyResponse, error) {
	// Reject writes to an empty queue/namespace once, before the WATCH/retry
	// loop, so an empty queue is never written.
	if err := validate.Modification(mod); err != nil {
		return nil, fmt.Errorf("eqredis modify: %w", err)
	}
	start := time.Now()
	defer func() { e.modifyDuration.Record(ctx, time.Since(start).Seconds()) }()
	for attempt := 0; ; attempt++ {
		resp, err := e.modifyOnce(ctx, mod)
		if !errors.Is(err, redis.TxFailedErr) {
			return resp, err
		}
		if err := waitToRetry(ctx, attempt); err != nil {
			return nil, fmt.Errorf("eqredis modify: %w", err)
		}
	}
}

// heldByOther returns true if the task is actively claimed by a different claimant.
// A task is considered held only when it has a claimant, that claimant is not
// the caller, and the claim has not yet expired (at > now).
func heldByOther(f *taskFields, callerClaimant string, nowMs int64) bool {
	return f.Claimant != "" && f.Claimant != callerClaimant && f.AtMs > nowMs
}

func (e *EQRedis) modifyOnce(ctx context.Context, mod *entroq.Modification) (*entroq.ModifyResponse, error) {
	now := time.Now().UTC()
	nowMs := now.UnixMilli()
	claimant := mod.Claimant

	// Collect all task keys that must exist and be watched.
	type watchedTask struct {
		id      string
		version int32
		queue   string // claimed current queue for depends and deletes, matched against stored state (changes match their FromQueue on mod.Changes directly)
	}

	var watchKeys []string
	var deps []watchedTask
	var dels []watchedTask

	for _, t := range mod.Depends {
		watchKeys = append(watchKeys, taskKey(t.ID))
		deps = append(deps, watchedTask{id: t.ID, version: t.Version, queue: t.Queue})
	}
	for _, t := range mod.Deletes {
		watchKeys = append(watchKeys, taskKey(t.ID))
		dels = append(dels, watchedTask{id: t.ID, version: t.Version, queue: t.Queue})
	}
	for _, t := range mod.Changes {
		watchKeys = append(watchKeys, taskKey(t.ID))
	}
	// Inserts with explicit IDs must be watched so we can detect collisions.
	for _, t := range mod.Inserts {
		if t.ID != "" {
			watchKeys = append(watchKeys, taskKey(t.ID))
		}
	}
	// Doc depends, deletes, changes, and explicit inserts also need to be watched.
	for _, d := range mod.DocDepends {
		watchKeys = append(watchKeys, docKey(d.Namespace, d.ID))
	}
	for _, d := range mod.DocDeletes {
		watchKeys = append(watchKeys, docKey(d.Namespace, d.ID))
	}
	for _, d := range mod.DocChanges {
		watchKeys = append(watchKeys, docKey(d.Namespace, d.ID))
	}
	for _, d := range mod.DocInserts {
		if d.ID != "" {
			watchKeys = append(watchKeys, docKey(d.Namespace, d.ID))
		}
	}
	// Every write to a doc group writes its lock, so watching the lock
	// serializes writers of a group. An insert's group is known now; the other
	// operations' groups come from their stored docs, watched once read.
	insertGroups := make(map[docgroup.Group]bool)
	for _, d := range mod.DocInserts {
		g := docgroup.Group{Namespace: d.Namespace, Key: d.Key}
		if !insertGroups[g] {
			insertGroups[g] = true
			watchKeys = append(watchKeys, lockKey(g))
		}
	}

	var resp entroq.ModifyResponse

	err := e.client.Watch(ctx, func(tx *redis.Tx) error {
		// Step 1: read current state of all watched tasks.
		type taskState struct {
			fields *taskFields
			found  bool
		}
		states := make(map[string]*taskState)

		pipe := tx.Pipeline()
		cmds := make(map[string]*redis.MapStringStringCmd)
		allIDs := make([]string, 0, len(deps)+len(dels)+len(mod.Changes)+len(mod.Inserts))
		for _, t := range deps {
			allIDs = append(allIDs, t.id)
		}
		for _, t := range dels {
			allIDs = append(allIDs, t.id)
		}
		for _, t := range mod.Changes {
			allIDs = append(allIDs, t.ID)
		}
		for _, t := range mod.Inserts {
			if t.ID != "" {
				allIDs = append(allIDs, t.ID)
			}
		}
		for _, id := range allIDs {
			cmds[id] = pipe.HGetAll(ctx, taskKey(id))
		}

		// Also read doc hashes for dep/delete/change version checks.
		type docState struct {
			fields *docFields
			found  bool
		}
		docStates := make(map[string]*docState)
		docCmds := make(map[string]*redis.MapStringStringCmd)
		for _, d := range mod.DocDepends {
			k := d.Namespace + "/" + d.ID
			docCmds[k] = pipe.HGetAll(ctx, docKey(d.Namespace, d.ID))
		}
		for _, d := range mod.DocDeletes {
			k := d.Namespace + "/" + d.ID
			if _, exists := docCmds[k]; !exists {
				docCmds[k] = pipe.HGetAll(ctx, docKey(d.Namespace, d.ID))
			}
		}
		for _, d := range mod.DocChanges {
			k := d.Namespace + "/" + d.ID
			if _, exists := docCmds[k]; !exists {
				docCmds[k] = pipe.HGetAll(ctx, docKey(d.Namespace, d.ID))
			}
		}
		for _, d := range mod.DocInserts {
			if d.ID == "" {
				continue
			}
			k := d.Namespace + "/" + d.ID
			if _, exists := docCmds[k]; !exists {
				docCmds[k] = pipe.HGetAll(ctx, docKey(d.Namespace, d.ID))
			}
		}

		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("pipeline hgetall: %w", err)
		}

		for id, cmd := range cmds {
			vals, err := cmd.Result()
			if err != nil && !errors.Is(err, redis.Nil) {
				return fmt.Errorf("hgetall %q: %w", id, err)
			}
			if len(vals) == 0 {
				states[id] = &taskState{found: false}
				continue
			}
			f, err := parseTaskFields(vals)
			if err != nil {
				return fmt.Errorf("parse task %q: %w", id, err)
			}
			states[id] = &taskState{fields: f, found: true}
		}
		for k, cmd := range docCmds {
			vals, err := cmd.Result()
			if err != nil && !errors.Is(err, redis.Nil) {
				return fmt.Errorf("hgetall doc %q: %w", k, err)
			}
			if len(vals) == 0 {
				docStates[k] = &docState{found: false}
				continue
			}
			f, err := parseDocFields(vals)
			if err != nil {
				return fmt.Errorf("parse doc %q: %w", k, err)
			}
			docStates[k] = &docState{fields: f, found: true}
		}

		// Step 2: verify versions -- semantic failure, no retry.
		depErr := &entroq.DependencyError{}

		// The queue is part of the modify key: an op must name the task's current
		// queue (a change names its FromQueue), or it fails like a missing task.
		// An empty or mismatched queue is rejected; there is no fill-in from
		// stored state, which is exactly what would defeat the check.
		for _, t := range deps {
			st := states[t.id]
			if !st.found {
				depErr.Depends = append(depErr.Depends, &entroq.TaskID{ID: t.id, Version: t.version})
			} else if st.fields.Version != t.version {
				depErr.Depends = append(depErr.Depends, &entroq.TaskID{ID: t.id, Version: t.version})
			} else if t.queue == "" || t.queue != st.fields.Queue {
				depErr.Depends = append(depErr.Depends, &entroq.TaskID{ID: t.id, Version: t.version, Queue: t.queue})
			}
		}
		for _, t := range dels {
			st := states[t.id]
			if !st.found {
				depErr.Deletes = append(depErr.Deletes, &entroq.TaskID{ID: t.id, Version: t.version})
			} else if st.fields.Version != t.version {
				depErr.Deletes = append(depErr.Deletes, &entroq.TaskID{ID: t.id, Version: t.version})
			} else if t.queue == "" || t.queue != st.fields.Queue {
				depErr.Deletes = append(depErr.Deletes, &entroq.TaskID{ID: t.id, Version: t.version, Queue: t.queue})
			} else if heldByOther(st.fields, mod.Claimant, nowMs) {
				depErr.Claims = append(depErr.Claims, &entroq.TaskID{ID: t.id, Version: t.version})
			}
		}
		for _, t := range mod.Changes {
			st := states[t.ID]
			if !st.found {
				depErr.Changes = append(depErr.Changes, &entroq.TaskID{ID: t.ID, Version: t.Version})
			} else if st.fields.Version != t.Version {
				depErr.Changes = append(depErr.Changes, &entroq.TaskID{ID: t.ID, Version: t.Version})
			} else if t.FromQueue == "" || t.FromQueue != st.fields.Queue {
				depErr.Changes = append(depErr.Changes, &entroq.TaskID{ID: t.ID, Version: t.Version, Queue: t.FromQueue})
			} else if heldByOther(st.fields, mod.Claimant, nowMs) {
				depErr.Claims = append(depErr.Claims, &entroq.TaskID{ID: t.ID, Version: t.Version})
			}
		}
		for _, t := range mod.Inserts {
			if t.ID == "" {
				continue
			}
			if st, ok := states[t.ID]; ok && st.found {
				depErr.Inserts = append(depErr.Inserts, &entroq.TaskID{ID: t.ID, Version: st.fields.Version})
			}
		}
		// Doc groups follow the rules in docgroup, against each group's lock.
		member := func(ns, id string) *entroq.Doc {
			if st := docStates[ns+"/"+id]; st != nil && st.found {
				return st.fields.toDoc()
			}
			return nil
		}
		groups := docgroup.Groups(mod, member)
		var storedLockKeys []string
		for _, g := range groups {
			if !insertGroups[g] {
				storedLockKeys = append(storedLockKeys, lockKey(g))
			}
		}
		if len(storedLockKeys) > 0 {
			if err := tx.Watch(ctx, storedLockKeys...).Err(); err != nil {
				return fmt.Errorf("watch doc locks: %w", err)
			}
		}
		locks, err := readLocks(ctx, tx, groups)
		if err != nil {
			return err
		}
		docPlan := docgroup.Evaluate(mod, now, member,
			func(g docgroup.Group) docgroup.Lock { return locks[g] },
		)
		if merged := depErr.Merge(docPlan.Err); merged != nil {
			depErr = merged
		}
		// withLock gives a written member its group's new lock, the only
		// version and claim a member has.
		withLock := func(f *docFields) *entroq.Doc {
			g := docgroup.Group{Namespace: f.Namespace, Key: f.KeyPrimary}
			l, ok := docPlan.Locks[g]
			if !ok {
				l = locks[g]
			}
			return docgroup.Overlay(f.toDoc(), l)
		}
		if depErr.HasAny() {
			return depErr
		}

		// Step 3: build and execute the MULTI block.
		resp = entroq.ModifyResponse{}

		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			// Deletes: remove task hash, queue ZSET entry, and claimed ZSET entry.
			for _, t := range dels {
				st := states[t.id]
				q := st.fields.Queue
				pipe.Del(ctx, taskKey(t.id))
				pipe.ZRem(ctx, queueKey(q), t.id)
				pipe.ZRem(ctx, qsclaimedKey(q), t.id)
				pipe.ZRem(ctx, qsclaimsKey(q), t.id)
				// Queue cleanup is handled by the GC goroutine.
			}

			// Changes: update task hash, update ZSET score, manage inflight and queue membership.
			for _, t := range mod.Changes {
				st := states[t.ID]
				oldQueue := st.fields.Queue
				// EnsureModifyKeys (called at Modify entry) rejects an empty change
				// destination, so t.Queue is non-empty here.
				newQueue := t.Queue
				// Cap a far-past arrival to now (backend Modify contract).
				newAtMs := entroq.NormalizeArrival(t.At, now).UnixMilli()

				// Future at: claim/renew (set claimant to modifier).
				// Past/zero at: release (clear claimant).
				newClaimant := ""
				if newAtMs > nowMs {
					newClaimant = claimant
				}

				f := &taskFields{
					ID:       t.ID,
					Queue:    newQueue,
					Value:    json.RawMessage(t.Value),
					AtMs:     newAtMs,
					Created:  st.fields.Created,
					Modified: nowMs,
					Claimant: newClaimant,
					Version:  st.fields.Version + 1,
					Claims:   st.fields.Claims,
					Attempt:  t.Attempt,
					Err:      t.Err,
				}

				pipe.HSet(ctx, taskKey(t.ID), f.toMap())
				pipe.ZAdd(ctx, queueKey(newQueue), redis.Z{Score: float64(newAtMs), Member: t.ID})
				pipe.SAdd(ctx, queuesKey, newQueue)

				if newQueue != oldQueue {
					pipe.ZRem(ctx, queueKey(oldQueue), t.ID)
					pipe.ZRem(ctx, qsclaimedKey(oldQueue), t.ID)
					pipe.ZRem(ctx, qsclaimsKey(oldQueue), t.ID)
					if f.Claims > 0 {
						pipe.ZAdd(ctx, qsclaimsKey(newQueue), redis.Z{Score: float64(f.Claims), Member: t.ID})
					}
				}
				// Claimed means not yet available and claimed at least once; a
				// task that was never claimed is future, however it got there.
				if newAtMs > nowMs && f.Claims > 0 {
					pipe.ZAdd(ctx, qsclaimedKey(newQueue), redis.Z{Score: float64(newAtMs), Member: t.ID})
				} else {
					pipe.ZRem(ctx, qsclaimedKey(newQueue), t.ID)
				}

				resp.ChangedTasks = append(resp.ChangedTasks, f.toTask())
			}

			// Inserts: create new task hashes and add to ZSETs.
			for _, td := range mod.Inserts {
				id := td.ID
				if id == "" {
					id = entroq.GenHex16()
				}
				atMs := entroq.NormalizeArrival(td.At, now).UnixMilli()
				// As for a change, the writer holds a task that is not yet
				// available.
				insClaimant := ""
				if atMs > nowMs {
					insClaimant = claimant
				}

				f := &taskFields{
					ID:       id,
					Queue:    td.Queue,
					Value:    json.RawMessage(td.Value),
					AtMs:     atMs,
					Created:  nowMs,
					Modified: nowMs,
					Claimant: insClaimant,
					Version:  0,
					Claims:   0,
					Attempt:  td.Attempt,
					Err:      td.Err,
				}

				pipe.HSet(ctx, taskKey(id), f.toMap())
				pipe.ZAdd(ctx, queueKey(td.Queue), redis.Z{Score: float64(atMs), Member: id})
				pipe.SAdd(ctx, queuesKey, td.Queue)

				resp.InsertedTasks = append(resp.InsertedTasks, f.toTask())
			}

			// Doc deletes.
			for _, d := range mod.DocDeletes {
				k := d.Namespace + "/" + d.ID
				st := docStates[k]
				pipe.Del(ctx, docKey(d.Namespace, d.ID))
				if st.found {
					pipe.ZRem(ctx, docNSIndexKey(d.Namespace),
						docIndexMember(st.fields.KeyPrimary, st.fields.KeySecondary, d.ID))
				}
			}

			// Doc inserts.
			for _, dd := range mod.DocInserts {
				if err := validateDocKeys(dd.Key, dd.SecondaryKey); err != nil {
					return err
				}
				id := dd.ID
				if id == "" {
					id = entroq.GenHex16()
				}
				f := &docFields{
					Namespace:    dd.Namespace,
					ID:           id,
					KeyPrimary:   dd.Key,
					KeySecondary: dd.SecondaryKey,
					Content:      []byte(dd.Content),
					Created:      nowMs,
					Modified:     nowMs,
				}
				pipe.HSet(ctx, docKey(dd.Namespace, id), f.toMap())
				pipe.ZAdd(ctx, docNSIndexKey(dd.Namespace), redis.Z{
					Score:  0,
					Member: docIndexMember(dd.Key, dd.SecondaryKey, id),
				})
				pipe.SAdd(ctx, namespacesKey, dd.Namespace)
				resp.InsertedDocs = append(resp.InsertedDocs, withLock(f))
			}

			// Doc changes replace content; keys and Created belong to the stored
			// doc, so its index entry does not move.
			for _, d := range mod.DocChanges {
				st := docStates[d.Namespace+"/"+d.ID]
				f := &docFields{
					Namespace:    d.Namespace,
					ID:           d.ID,
					KeyPrimary:   st.fields.KeyPrimary,
					KeySecondary: st.fields.KeySecondary,
					Content:      []byte(d.Content),
					Created:      st.fields.Created,
					Modified:     nowMs,
				}
				pipe.HSet(ctx, docKey(d.Namespace, d.ID), f.toMap())
				resp.ChangedDocs = append(resp.ChangedDocs, withLock(f))
			}

			for g, l := range docPlan.Locks {
				writeLock(ctx, pipe, g, l, now)
			}

			return nil
		})
		return err
	}, watchKeys...)

	// A DependencyError is returned as-is through Watch.
	if depErr, ok := err.(*entroq.DependencyError); ok {
		return nil, depErr
	}
	if err != nil {
		return nil, fmt.Errorf("eqredis modify: %w", err)
	}

	// Notify queues that had inserts or changes with at <= now.
	entroq.NotifyModified(e.nw, resp.InsertedTasks, resp.ChangedTasks)

	return &resp, nil
}
