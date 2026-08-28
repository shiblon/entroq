package eqsqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
	if err := deleteTasks(ctx, tx, mod.Deletes); err != nil {
		return nil, err
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
		resp.ChangedTasks = append(resp.ChangedTasks, updated)
	}
	if err := changeTasks(ctx, tx, resp.ChangedTasks); err != nil {
		return nil, err
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
		resp.InsertedTasks = append(resp.InsertedTasks, task)
	}
	if err := insertTasks(ctx, tx, resp.InsertedTasks); err != nil {
		return nil, err
	}

	if err := deleteDocs(ctx, tx, mod.DocDeletes); err != nil {
		return nil, err
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
		resp.InsertedDocs = append(resp.InsertedDocs, doc)
	}
	if err := insertDocs(ctx, tx, resp.InsertedDocs); err != nil {
		return nil, err
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
		resp.ChangedDocs = append(resp.ChangedDocs, doc)
	}
	if err := changeDocs(ctx, tx, resp.ChangedDocs); err != nil {
		return nil, err
	}
	return resp, nil
}

// modernc SQLite is built with SQLITE_MAX_VARIABLE_NUMBER=32766. Chunking at
// that boundary keeps large public Modify calls valid without returning to one
// statement per row.
const sqliteMaxVariables = 32766

func batchRanges(length, variablesPerRow int, f func(start, end int) error) error {
	if length == 0 {
		return nil
	}
	batchSize := sqliteMaxVariables / variablesPerRow
	for start := 0; start < length; start += batchSize {
		if err := f(start, min(start+batchSize, length)); err != nil {
			return err
		}
	}
	return nil
}

func rowPlaceholders(rows, columns int) string {
	row := "(" + placeholders(columns) + "),"
	return strings.TrimSuffix(strings.Repeat(row, rows), ",")
}

func deleteTasks(ctx context.Context, tx *sql.Tx, deletes []*entroq.TaskID) error {
	return batchRanges(len(deletes), 1, func(start, end int) error {
		args := make([]any, 0, end-start)
		for _, task := range deletes[start:end] {
			args = append(args, task.ID)
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM tasks WHERE id IN ("+placeholders(len(args))+")", args...); err != nil {
			return fmt.Errorf("delete tasks: %w", err)
		}
		return nil
	})
}

func changeTasks(ctx context.Context, tx *sql.Tx, tasks []*entroq.Task) error {
	const columns = 9
	return batchRanges(len(tasks), columns, func(start, end int) error {
		args := make([]any, 0, columns*(end-start))
		for _, task := range tasks[start:end] {
			args = append(args, task.ID, task.Version, task.Queue, task.At.UnixMilli(),
				task.Claimant, jsonValue(task.Value), task.Modified.UnixMilli(), task.Attempt, task.Err)
		}
		query := `WITH changes(id, new_version, new_queue, new_at_ms, new_claimant,
			new_value, new_modified_ms, new_attempt, new_err) AS (VALUES ` +
			rowPlaceholders(end-start, columns) + `)
			UPDATE tasks SET
				version = changes.new_version,
				queue = changes.new_queue,
				at_ms = changes.new_at_ms,
				claimant = changes.new_claimant,
				value = changes.new_value,
				modified_ms = changes.new_modified_ms,
				attempt = changes.new_attempt,
				err = changes.new_err
			FROM changes WHERE tasks.id = changes.id`
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("change tasks: %w", err)
		}
		return nil
	})
}

func insertTasks(ctx context.Context, tx *sql.Tx, tasks []*entroq.Task) error {
	const columns = 11
	return batchRanges(len(tasks), columns, func(start, end int) error {
		args := make([]any, 0, columns*(end-start))
		for _, task := range tasks[start:end] {
			args = append(args, task.ID, task.Version, task.Queue, task.At.UnixMilli(), task.Claimant,
				task.Claims, jsonValue(task.Value), task.Created.UnixMilli(), task.Modified.UnixMilli(),
				task.Attempt, task.Err)
		}
		query := `INSERT INTO tasks
			(id, version, queue, at_ms, claimant, claims, value, created_ms, modified_ms, attempt, err)
			VALUES ` + rowPlaceholders(end-start, columns)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("insert tasks: %w", err)
		}
		return nil
	})
}

func deleteDocs(ctx context.Context, tx *sql.Tx, deletes []*entroq.DocID) error {
	const columns = 2
	return batchRanges(len(deletes), columns, func(start, end int) error {
		args := make([]any, 0, columns*(end-start))
		for _, doc := range deletes[start:end] {
			args = append(args, doc.Namespace, doc.ID)
		}
		query := "DELETE FROM docs WHERE (namespace, id) IN (VALUES " +
			rowPlaceholders(end-start, columns) + ")"
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("delete docs: %w", err)
		}
		return nil
	})
}

func insertDocs(ctx context.Context, tx *sql.Tx, docs []*entroq.Doc) error {
	const columns = 8
	return batchRanges(len(docs), columns, func(start, end int) error {
		args := make([]any, 0, columns*(end-start))
		for _, doc := range docs[start:end] {
			args = append(args, doc.Namespace, doc.ID, doc.Version, doc.Key, doc.SecondaryKey,
				jsonValue(doc.Content), doc.Created.UnixMilli(), doc.Modified.UnixMilli())
		}
		query := `INSERT INTO docs
			(namespace, id, version, claimant, at_ms, key_primary, key_secondary, content, created_ms, modified_ms)
			VALUES `
		row := "(?, ?, ?, '', 0, ?, ?, ?, ?, ?),"
		query += strings.TrimSuffix(strings.Repeat(row, end-start), ",")
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("insert docs: %w", err)
		}
		return nil
	})
}

func changeDocs(ctx context.Context, tx *sql.Tx, docs []*entroq.Doc) error {
	const columns = 9
	return batchRanges(len(docs), columns, func(start, end int) error {
		args := make([]any, 0, columns*(end-start))
		for _, doc := range docs[start:end] {
			args = append(args, doc.Namespace, doc.ID, doc.Version, doc.Claimant, doc.At.UnixMilli(),
				doc.Key, doc.SecondaryKey, jsonValue(doc.Content), doc.Modified.UnixMilli())
		}
		query := `WITH changes(namespace, id, new_version, new_claimant, new_at_ms,
			new_key_primary, new_key_secondary, new_content, new_modified_ms) AS (VALUES ` +
			rowPlaceholders(end-start, columns) + `)
			UPDATE docs SET
				version = changes.new_version,
				claimant = changes.new_claimant,
				at_ms = changes.new_at_ms,
				key_primary = changes.new_key_primary,
				key_secondary = changes.new_key_secondary,
				content = changes.new_content,
				modified_ms = changes.new_modified_ms
			FROM changes
			WHERE docs.namespace = changes.namespace AND docs.id = changes.id`
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("change docs: %w", err)
		}
		return nil
	})
}

func loadDependencies(ctx context.Context, tx *sql.Tx, mod *entroq.Modification) (map[string]*entroq.Task, map[string]*entroq.Doc, error) {
	taskDeps, docDeps, _ := mod.AllDependencies()
	tasks := make(map[string]*entroq.Task, len(taskDeps))
	taskIDs := make([]string, 0, len(taskDeps))
	for id := range taskDeps {
		taskIDs = append(taskIDs, id)
	}
	if err := batchRanges(len(taskIDs), 1, func(start, end int) error {
		args := make([]any, end-start)
		for i, id := range taskIDs[start:end] {
			args[i] = id
		}
		rows, err := tx.QueryContext(ctx,
			"SELECT "+taskColumns+" FROM tasks WHERE id IN ("+placeholders(len(args))+")", args...)
		if err != nil {
			return fmt.Errorf("load task dependencies: %w", err)
		}
		for rows.Next() {
			task, err := scanTask(rows)
			if err != nil {
				rows.Close()
				return fmt.Errorf("scan task dependency: %w", err)
			}
			tasks[task.ID] = task
		}
		err = rows.Err()
		rows.Close()
		return err
	}); err != nil {
		return nil, nil, err
	}

	docs := make(map[string]*entroq.Doc, len(docDeps))
	type docKey struct{ namespace, id string }
	docIDs := make([]docKey, 0, len(docDeps))
	seenDocs := make(map[string]bool, len(docDeps))
	addDoc := func(namespace, id string) {
		key := entroq.DocKey(namespace, id)
		if !seenDocs[key] {
			seenDocs[key] = true
			docIDs = append(docIDs, docKey{namespace: namespace, id: id})
		}
	}
	for _, change := range mod.DocChanges {
		addDoc(change.Namespace, change.ID)
	}
	for _, dep := range mod.DocDepends {
		addDoc(dep.Namespace, dep.ID)
	}
	for _, del := range mod.DocDeletes {
		addDoc(del.Namespace, del.ID)
	}
	for _, insert := range mod.DocInserts {
		if insert.ID != "" {
			addDoc(insert.Namespace, insert.ID)
		}
	}
	if err := batchRanges(len(docIDs), 2, func(start, end int) error {
		args := make([]any, 0, 2*(end-start))
		for _, doc := range docIDs[start:end] {
			args = append(args, doc.namespace, doc.id)
		}
		query := "SELECT " + docColumns + " FROM docs WHERE (namespace, id) IN (VALUES " +
			rowPlaceholders(end-start, 2) + ")"
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("load doc dependencies: %w", err)
		}
		for rows.Next() {
			doc, err := scanDoc(rows)
			if err != nil {
				rows.Close()
				return fmt.Errorf("scan doc dependency: %w", err)
			}
			docs[entroq.DocKey(doc.Namespace, doc.ID)] = doc
		}
		err = rows.Err()
		rows.Close()
		return err
	}); err != nil {
		return nil, nil, err
	}
	return tasks, docs, nil
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
