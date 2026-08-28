package eqsqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/shiblon/entroq"
)

// Modify atomically applies a modification to the task and document store.
func (b *EQSQLite) Modify(ctx context.Context, mod *entroq.Modification) (*entroq.ModifyResponse, error) {
	start := time.Now()
	defer func() { b.modifyDur.Record(ctx, time.Since(start).Seconds()) }()
	if mod == nil {
		return nil, fmt.Errorf("eqsqlite modify: nil modification")
	}
	if err := mod.EnsureModifyKeys(); err != nil {
		return nil, fmt.Errorf("eqsqlite modify: %w", err)
	}
	if _, _, err := mod.AllDependencies(); err != nil {
		return nil, fmt.Errorf("eqsqlite modify: %w", err)
	}
	value, err := b.write(ctx, func(ctx context.Context, tx *sql.Tx) (any, error) {
		return modifyTx(ctx, tx, mod)
	})
	if err != nil {
		return nil, fmt.Errorf("eqsqlite modify: %w", err)
	}
	resp := value.(*entroq.ModifyResponse)
	entroq.NotifyModified(b.nw, resp.InsertedTasks, resp.ChangedTasks)
	return resp, nil
}

func modifyTx(ctx context.Context, tx *sql.Tx, mod *entroq.Modification) (*entroq.ModifyResponse, error) {
	now := nowUTC()
	foundTasks, foundDocs, err := loadDependencies(ctx, tx, mod)
	if err != nil {
		return nil, err
	}
	if depErr := checkDependencies(mod, foundTasks, foundDocs, now); depErr.HasAny() {
		return nil, depErr
	}

	resp := &entroq.ModifyResponse{}
	for _, del := range mod.Deletes {
		if _, err := tx.ExecContext(ctx, "DELETE FROM tasks WHERE id = ?", del.ID); err != nil {
			return nil, fmt.Errorf("delete task %q: %w", del.ID, err)
		}
	}
	for _, change := range mod.Changes {
		old := foundTasks[change.ID]
		at := entroq.NormalizeArrival(change.At, now)
		claimant := ""
		if at.After(now) {
			claimant = mod.Claimant
		}
		updated := &entroq.Task{
			ID: change.ID, Version: old.Version + 1, Queue: change.Queue,
			At: at, Claimant: claimant, Claims: old.Claims, Value: change.Value,
			Created: old.Created, Modified: now, Attempt: change.Attempt, Err: change.Err,
		}
		_, err := tx.ExecContext(ctx, `UPDATE tasks SET
            version = ?, queue = ?, at_ms = ?, claimant = ?, value = ?,
            modified_ms = ?, attempt = ?, err = ? WHERE id = ?`,
			updated.Version, updated.Queue, updated.At.UnixMilli(), updated.Claimant,
			jsonValue(updated.Value), updated.Modified.UnixMilli(), updated.Attempt,
			updated.Err, updated.ID)
		if err != nil {
			return nil, fmt.Errorf("change task %q: %w", change.ID, err)
		}
		resp.ChangedTasks = append(resp.ChangedTasks, updated)
	}
	for _, insert := range mod.Inserts {
		id := insert.ID
		if id == "" {
			id = entroq.GenHex16()
		}
		at := entroq.NormalizeArrival(insert.At, now)
		created := time.UnixMilli(storedTime(insert.Created, now)).UTC()
		modified := time.UnixMilli(storedTime(insert.Modified, now)).UTC()
		task := &entroq.Task{
			ID: id, Queue: insert.Queue, Version: 0, At: at,
			Claimant: mod.Claimant, Value: insert.Value, Created: created,
			Modified: modified, Attempt: insert.Attempt, Err: insert.Err,
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO tasks
            (id, version, queue, at_ms, claimant, claims, value, created_ms, modified_ms, attempt, err)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			task.ID, task.Version, task.Queue, task.At.UnixMilli(), task.Claimant,
			task.Claims, jsonValue(task.Value), task.Created.UnixMilli(),
			task.Modified.UnixMilli(), task.Attempt, task.Err)
		if err != nil {
			return nil, fmt.Errorf("insert task %q: %w", task.ID, err)
		}
		resp.InsertedTasks = append(resp.InsertedTasks, task)
	}

	for _, del := range mod.DocDeletes {
		if _, err := tx.ExecContext(ctx, "DELETE FROM docs WHERE namespace = ? AND id = ?", del.Namespace, del.ID); err != nil {
			return nil, fmt.Errorf("delete doc %q/%q: %w", del.Namespace, del.ID, err)
		}
	}
	for _, insert := range mod.DocInserts {
		id := insert.ID
		if id == "" {
			id = entroq.GenHex16()
		}
		created := time.UnixMilli(storedTime(insert.Created, now)).UTC()
		modified := time.UnixMilli(storedTime(insert.Modified, now)).UTC()
		doc := &entroq.Doc{
			Namespace: insert.Namespace, ID: id, Version: 0,
			Key: insert.Key, SecondaryKey: insert.SecondaryKey, Content: insert.Content,
			Created: created, Modified: modified,
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO docs
            (namespace, id, version, claimant, at_ms, key_primary, key_secondary, content, created_ms, modified_ms)
            VALUES (?, ?, ?, '', 0, ?, ?, ?, ?, ?)`,
			doc.Namespace, doc.ID, doc.Version, doc.Key, doc.SecondaryKey,
			jsonValue(doc.Content), doc.Created.UnixMilli(), doc.Modified.UnixMilli())
		if err != nil {
			return nil, fmt.Errorf("insert doc %q/%q: %w", doc.Namespace, doc.ID, err)
		}
		resp.InsertedDocs = append(resp.InsertedDocs, doc)
	}
	for _, change := range mod.DocChanges {
		old := foundDocs[entroq.DocKey(change.Namespace, change.ID)]
		at := entroq.NormalizeArrival(change.At, now)
		claimant := ""
		if at.After(now) {
			claimant = mod.Claimant
		}
		doc := &entroq.Doc{
			Namespace: change.Namespace, ID: change.ID, Version: old.Version + 1,
			Claimant: claimant, At: at, Key: change.Key, SecondaryKey: change.SecondaryKey,
			Content: change.Content, Created: old.Created, Modified: now,
		}
		_, err := tx.ExecContext(ctx, `UPDATE docs SET
            version = ?, claimant = ?, at_ms = ?, key_primary = ?, key_secondary = ?,
            content = ?, modified_ms = ? WHERE namespace = ? AND id = ?`,
			doc.Version, doc.Claimant, doc.At.UnixMilli(), doc.Key, doc.SecondaryKey,
			jsonValue(doc.Content), doc.Modified.UnixMilli(), doc.Namespace, doc.ID)
		if err != nil {
			return nil, fmt.Errorf("change doc %q/%q: %w", doc.Namespace, doc.ID, err)
		}
		resp.ChangedDocs = append(resp.ChangedDocs, doc)
	}
	return resp, nil
}

func loadDependencies(ctx context.Context, tx *sql.Tx, mod *entroq.Modification) (map[string]*entroq.Task, map[string]*entroq.Doc, error) {
	taskDeps, docDeps, _ := mod.AllDependencies()
	tasks := make(map[string]*entroq.Task, len(taskDeps))
	for id := range taskDeps {
		task, err := scanTask(tx.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = ?", id))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("load task dependency %q: %w", id, err)
		}
		tasks[id] = task
	}
	docs := make(map[string]*entroq.Doc, len(docDeps))
	for _, change := range mod.DocChanges {
		if _, ok := docs[entroq.DocKey(change.Namespace, change.ID)]; !ok {
			if err := loadOneDoc(ctx, tx, docs, change.Namespace, change.ID); err != nil {
				return nil, nil, err
			}
		}
	}
	for _, dep := range append(append([]*entroq.DocID{}, mod.DocDepends...), mod.DocDeletes...) {
		if _, ok := docs[entroq.DocKey(dep.Namespace, dep.ID)]; !ok {
			if err := loadOneDoc(ctx, tx, docs, dep.Namespace, dep.ID); err != nil {
				return nil, nil, err
			}
		}
	}
	for _, insert := range mod.DocInserts {
		if insert.ID != "" {
			if err := loadOneDoc(ctx, tx, docs, insert.Namespace, insert.ID); err != nil {
				return nil, nil, err
			}
		}
	}
	return tasks, docs, nil
}

func loadOneDoc(ctx context.Context, tx *sql.Tx, docs map[string]*entroq.Doc, namespace, id string) error {
	doc, err := scanDoc(tx.QueryRowContext(ctx, "SELECT "+docColumns+" FROM docs WHERE namespace = ? AND id = ?", namespace, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load doc dependency %q/%q: %w", namespace, id, err)
	}
	docs[entroq.DocKey(namespace, id)] = doc
	return nil
}

func checkDependencies(mod *entroq.Modification, tasks map[string]*entroq.Task, docs map[string]*entroq.Doc, now time.Time) *entroq.DependencyError {
	depErr := &entroq.DependencyError{}
	for _, dep := range mod.Depends {
		found := tasks[dep.ID]
		if found == nil || found.Version != dep.Version || dep.Queue == "" || found.Queue != dep.Queue {
			depErr.Depends = append(depErr.Depends, &entroq.TaskID{ID: dep.ID, Version: dep.Version, Queue: dep.Queue})
		}
	}
	for _, del := range mod.Deletes {
		found := tasks[del.ID]
		if found == nil || found.Version != del.Version || del.Queue == "" || found.Queue != del.Queue {
			depErr.Deletes = append(depErr.Deletes, &entroq.TaskID{ID: del.ID, Version: del.Version, Queue: del.Queue})
		} else if heldTask(found, mod.Claimant, now) {
			depErr.Claims = append(depErr.Claims, del)
		}
	}
	for _, change := range mod.Changes {
		found := tasks[change.ID]
		if found == nil || found.Version != change.Version || change.FromQueue == "" || found.Queue != change.FromQueue {
			depErr.Changes = append(depErr.Changes, &entroq.TaskID{ID: change.ID, Version: change.Version, Queue: change.FromQueue})
		} else if heldTask(found, mod.Claimant, now) {
			depErr.Claims = append(depErr.Claims, &entroq.TaskID{ID: change.ID, Version: change.Version, Queue: change.FromQueue})
		}
	}
	for _, insert := range mod.Inserts {
		if found := tasks[insert.ID]; insert.ID != "" && found != nil {
			depErr.Inserts = append(depErr.Inserts, found.IDVersion())
		}
	}
	for _, dep := range mod.DocDepends {
		found := docs[entroq.DocKey(dep.Namespace, dep.ID)]
		if found == nil || found.Version != dep.Version {
			depErr.DocDepends = append(depErr.DocDepends, dep)
		}
	}
	for _, del := range mod.DocDeletes {
		found := docs[entroq.DocKey(del.Namespace, del.ID)]
		if found == nil || found.Version != del.Version {
			depErr.DocDeletes = append(depErr.DocDeletes, del)
		} else if heldDoc(found, mod.Claimant, now) {
			depErr.DocClaims = append(depErr.DocClaims, del)
		}
	}
	for _, change := range mod.DocChanges {
		found := docs[entroq.DocKey(change.Namespace, change.ID)]
		id := &entroq.DocID{Namespace: change.Namespace, ID: change.ID, Version: change.Version}
		if found == nil || found.Version != change.Version {
			depErr.DocChanges = append(depErr.DocChanges, id)
		} else if heldDoc(found, mod.Claimant, now) {
			depErr.DocClaims = append(depErr.DocClaims, id)
		}
	}
	for _, insert := range mod.DocInserts {
		if found := docs[entroq.DocKey(insert.Namespace, insert.ID)]; insert.ID != "" && found != nil {
			depErr.DocInserts = append(depErr.DocInserts, found.IDVersion())
		}
	}
	return depErr
}

func heldTask(task *entroq.Task, claimant string, now time.Time) bool {
	return task.Claimant != "" && task.Claimant != claimant && task.At.After(now)
}

func heldDoc(doc *entroq.Doc, claimant string, now time.Time) bool {
	return doc.Claimant != "" && doc.Claimant != claimant && doc.At.After(now)
}
