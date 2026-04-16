package eqredis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shiblon/entroq"
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
	for {
		resp, err := e.modifyOnce(ctx, mod)
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		return resp, err
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
		queue   string // known for deletes and changes; empty for depends
	}

	var watchKeys []string
	var deps []watchedTask
	var dels []watchedTask

	for _, t := range mod.Depends {
		watchKeys = append(watchKeys, taskKey(t.ID))
		deps = append(deps, watchedTask{id: t.ID, version: t.Version})
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

		for _, t := range deps {
			st := states[t.id]
			if !st.found {
				depErr.Depends = append(depErr.Depends, &entroq.TaskID{ID: t.id, Version: t.version})
			} else if st.fields.Version != t.version {
				depErr.Depends = append(depErr.Depends, &entroq.TaskID{ID: t.id, Version: t.version})
			}
		}
		for _, t := range dels {
			st := states[t.id]
			if !st.found {
				depErr.Deletes = append(depErr.Deletes, &entroq.TaskID{ID: t.id, Version: t.version})
			} else if st.fields.Version != t.version {
				depErr.Deletes = append(depErr.Deletes, &entroq.TaskID{ID: t.id, Version: t.version})
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
		for _, d := range mod.DocDepends {
			k := d.Namespace + "/" + d.ID
			st := docStates[k]
			if !st.found || st.fields.Version != d.Version {
				depErr.DocDepends = append(depErr.DocDepends, &entroq.DocID{Namespace: d.Namespace, ID: d.ID, Version: d.Version})
			}
		}
		for _, d := range mod.DocDeletes {
			k := d.Namespace + "/" + d.ID
			st := docStates[k]
			if !st.found || st.fields.Version != d.Version {
				depErr.DocDeletes = append(depErr.DocDeletes, &entroq.DocID{Namespace: d.Namespace, ID: d.ID, Version: d.Version})
			}
		}
		for _, d := range mod.DocChanges {
			k := d.Namespace + "/" + d.ID
			st := docStates[k]
			if !st.found || st.fields.Version != d.Version {
				depErr.DocChanges = append(depErr.DocChanges, &entroq.DocID{Namespace: d.Namespace, ID: d.ID, Version: d.Version})
			}
		}
		for _, d := range mod.DocInserts {
			if d.ID == "" {
				continue
			}
			k := d.Namespace + "/" + d.ID
			if st, ok := docStates[k]; ok && st.found {
				depErr.DocInserts = append(depErr.DocInserts, &entroq.DocID{Namespace: d.Namespace, ID: d.ID, Version: st.fields.Version})
			}
		}
		if depErr.HasAny() {
			return depErr
		}

		// Step 3: build and execute the MULTI block.
		resp = entroq.ModifyResponse{}

		_, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			// Deletes: remove task hash, remove from queue ZSET and inflight set.
			for _, t := range dels {
				st := states[t.id]
				q := st.fields.Queue
				pipe.Del(ctx, taskKey(t.id))
				pipe.ZRem(ctx, queueKey(q), t.id)
				pipe.SRem(ctx, inflightKey(q), t.id)
				// Queue cleanup is handled by the GC goroutine.
			}

			// Changes: update task hash, update ZSET score, manage inflight and queue membership.
			for _, t := range mod.Changes {
				st := states[t.ID]
				oldQueue := st.fields.Queue
				newQueue := t.Queue
				if newQueue == "" {
					newQueue = oldQueue
				}
				newAtMs := t.At.UnixMilli()
				if t.At.IsZero() {
					newAtMs = nowMs
				}

				// Preserve claimant if the task is being pushed to the future
				// (renewal). Clear it when At <= now so the task is available
				// to any claimant.
				newClaimant := ""
				if newAtMs > nowMs {
					newClaimant = st.fields.Claimant
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
					pipe.SRem(ctx, inflightKey(oldQueue), t.ID)
				} else {
					pipe.SRem(ctx, inflightKey(newQueue), t.ID)
				}

				resp.ChangedTasks = append(resp.ChangedTasks, f.toTask())
			}

			// Inserts: create new task hashes and add to ZSETs.
			for _, td := range mod.Inserts {
				id := td.ID
				if id == "" {
					id = entroq.GenHex16()
				}
				atMs := (td.At).UnixMilli()
				if td.At.IsZero() {
					atMs = nowMs
				}

				f := &taskFields{
					ID:       id,
					Queue:    td.Queue,
					Value:    json.RawMessage(td.Value),
					AtMs:     atMs,
					Created:  nowMs,
					Modified: nowMs,
					Claimant: claimant,
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
					Version:      0,
					Claimant:     "",
					AtMs:         0,
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
				resp.InsertedDocs = append(resp.InsertedDocs, f.toDoc())
			}

			// Doc changes.
			for _, d := range mod.DocChanges {
				if err := validateDocKeys(d.Key, d.SecondaryKey); err != nil {
					return err
				}
				k := d.Namespace + "/" + d.ID
				st := docStates[k]
				newAtMs := int64(0)
				if !d.At.IsZero() {
					newAtMs = (d.At).UnixMilli()
				}
				newClaimant := ""
				if newAtMs > nowMs {
					newClaimant = claimant
				}
				createdMs := nowMs
				if st.found {
					createdMs = st.fields.Created
				}
				f := &docFields{
					Namespace:    d.Namespace,
					ID:           d.ID,
					Version:      d.Version + 1,
					Claimant:     newClaimant,
					AtMs:         newAtMs,
					KeyPrimary:   d.Key,
					KeySecondary: d.SecondaryKey,
					Content:      []byte(d.Content),
					Created:      createdMs,
					Modified:     nowMs,
				}
				pipe.HSet(ctx, docKey(d.Namespace, d.ID), f.toMap())
				// Update namespace index: remove old member if key changed, add new.
				if st.found {
					oldMember := docIndexMember(st.fields.KeyPrimary, st.fields.KeySecondary, d.ID)
					newMember := docIndexMember(d.Key, d.SecondaryKey, d.ID)
					if oldMember != newMember {
						pipe.ZRem(ctx, docNSIndexKey(d.Namespace), oldMember)
					}
				}
				pipe.ZAdd(ctx, docNSIndexKey(d.Namespace), redis.Z{
					Score:  0,
					Member: docIndexMember(d.Key, d.SecondaryKey, d.ID),
				})
				resp.ChangedDocs = append(resp.ChangedDocs, f.toDoc())
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
