package eqsqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/arrival"
	"github.com/shiblon/entroq/pkg/backend/internal/docset"
	"github.com/shiblon/entroq/pkg/backend/internal/validate"
)

// Modify atomically applies a modification to the task and document store.
func (b *EQSQLite) Modify(ctx context.Context, mod *entroq.Modification) (*entroq.ModifyResponse, error) {
	start := time.Now()
	defer func() { b.modifyDur.Record(ctx, time.Since(start).Seconds()) }()
	if mod == nil {
		return nil, fmt.Errorf("eqsqlite modify: nil modification")
	}
	if err := validate.Modification(mod); err != nil {
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
	// Task arrivals are changes of the stored tasks' arrival times alone.
	mod = arrival.Changes(mod, func(id string) *entroq.Task { return foundTasks[id] })
	member := func(ns, id string) *entroq.Doc { return foundDocs[entroq.DocKey(ns, id)] }
	locks, err := loadDocLocks(ctx, tx, docset.Sets(mod, member))
	if err != nil {
		return nil, err
	}
	docPlan := docset.Evaluate(mod, now, member,
		func(g docset.Set) docset.Lock { return lockOf(locks, g) },
	)
	if depErr := checkDependencies(mod, foundTasks, now).Merge(docPlan.Err); depErr != nil {
		return nil, depErr
	}
	// Each written member carries its set's new lock, the only version and
	// claim a member has.
	withLock := func(d *entroq.Doc) {
		l, ok := docPlan.Locks[docset.Set{Namespace: d.Namespace, Key: d.Key}]
		if !ok {
			l = lockOf(locks, docset.Set{Namespace: d.Namespace, Key: d.Key})
		}
		d.Version, d.Claimant, d.At = l.Version, l.Claimant, l.At
	}

	resp := &entroq.ModifyResponse{}
	if err := deleteTasks(ctx, tx, mod.Deletes); err != nil {
		return nil, err
	}
	for _, change := range mod.Changes {
		old := foundTasks[change.ID]
		at := storedAt(change.By(), now)
		claimant := ""
		if at.After(now) {
			claimant = mod.Claimant
		}
		// A change keeps the claim count unless it resets it.
		claims := old.Claims
		if mod.ResetsClaims(change.ID) {
			claims = 0
		}
		updated := &entroq.Task{
			ID: change.ID, Version: old.Version + 1, Queue: change.Queue,
			At: at, Claimant: claimant, Claims: claims, Value: change.Value,
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
		at := storedAt(insert.By(), now)
		created := time.UnixMilli(storedTime(insert.Created, now)).UTC()
		modified := time.UnixMilli(storedTime(insert.Modified, now)).UTC()
		// As for a change, the writer holds a task that is not yet available.
		claimant := ""
		if at.After(now) {
			claimant = mod.Claimant
		}
		task := &entroq.Task{
			ID: id, Queue: insert.Queue, Version: 0, At: at,
			Claimant: claimant, Value: insert.Value, Created: created,
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
			Namespace: insert.Namespace, ID: id,
			Key: insert.Key, SecondaryKey: insert.SecondaryKey, Content: insert.Content,
			Created: created, Modified: modified,
		}
		withLock(doc)
		resp.InsertedDocs = append(resp.InsertedDocs, doc)
	}
	if err := insertDocs(ctx, tx, resp.InsertedDocs); err != nil {
		return nil, err
	}
	for _, change := range mod.DocChanges {
		// Keys and Created belong to the stored doc: a change replaces content.
		old := foundDocs[entroq.DocKey(change.Namespace, change.ID)]
		doc := &entroq.Doc{
			Namespace: change.Namespace, ID: change.ID,
			Key: old.Key, SecondaryKey: old.SecondaryKey,
			Content: change.Content, Created: old.Created, Modified: now,
		}
		withLock(doc)
		resp.ChangedDocs = append(resp.ChangedDocs, doc)
	}
	if err := changeDocs(ctx, tx, resp.ChangedDocs); err != nil {
		return nil, err
	}
	if err := saveDocLocks(ctx, tx, docPlan.Locks); err != nil {
		return nil, err
	}
	resp.ChangedSets = docPlan.Arrived
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
	const columns = 10
	return batchRanges(len(tasks), columns, func(start, end int) error {
		args := make([]any, 0, columns*(end-start))
		for _, task := range tasks[start:end] {
			args = append(args, task.ID, task.Version, task.Queue, task.At.UnixMilli(),
				task.Claimant, task.Claims, jsonValue(task.Value), task.Modified.UnixMilli(), task.Attempt, task.Err)
		}
		query := `WITH changes(id, new_version, new_queue, new_at_ms, new_claimant,
			new_claims, new_value, new_modified_ms, new_attempt, new_err) AS (VALUES ` +
			rowPlaceholders(end-start, columns) + `)
			UPDATE tasks SET
				version = changes.new_version,
				queue = changes.new_queue,
				at_ms = changes.new_at_ms,
				claimant = changes.new_claimant,
				claims = changes.new_claims,
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

// insertDocs writes new docs. A doc's version and claim are its set's,
// written to doc_locks.
func insertDocs(ctx context.Context, tx *sql.Tx, docs []*entroq.Doc) error {
	const columns = 7
	return batchRanges(len(docs), columns, func(start, end int) error {
		args := make([]any, 0, columns*(end-start))
		for _, doc := range docs[start:end] {
			args = append(args, doc.Namespace, doc.ID, doc.Key, doc.SecondaryKey,
				jsonValue(doc.Content), doc.Created.UnixMilli(), doc.Modified.UnixMilli())
		}
		query := `INSERT INTO docs
			(namespace, id, key_primary, key_secondary, content, created_ms, modified_ms)
			VALUES ` + rowPlaceholders(end-start, columns)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("insert docs: %w", err)
		}
		return nil
	})
}

// changeDocs replaces docs' content; keys belong to the stored doc, and the
// version and claim to its set's lock.
func changeDocs(ctx context.Context, tx *sql.Tx, docs []*entroq.Doc) error {
	const columns = 4
	return batchRanges(len(docs), columns, func(start, end int) error {
		args := make([]any, 0, columns*(end-start))
		for _, doc := range docs[start:end] {
			args = append(args, doc.Namespace, doc.ID, jsonValue(doc.Content), doc.Modified.UnixMilli())
		}
		query := `WITH changes(namespace, id, new_content, new_modified_ms) AS (VALUES ` +
			rowPlaceholders(end-start, columns) + `)
			UPDATE docs SET
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
		query := "SELECT " + docColumns + " FROM " + docsWithLocks + " WHERE (d.namespace, d.id) IN (VALUES " +
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

// checkDependencies checks mod's task operations against the stored tasks;
// docset checks its doc operations.
func checkDependencies(mod *entroq.Modification, tasks map[string]*entroq.Task, now time.Time) *entroq.DependencyError {
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
	return depErr
}

func heldTask(task *entroq.Task, claimant string, now time.Time) bool {
	return task.Claimant != "" && task.Claimant != claimant && task.At.After(now)
}

// loadDocLocks reads the locks of sets; a set with none is absent from the
// result.
func loadDocLocks(ctx context.Context, q queryer, sets []docset.Set) (map[docset.Set]docset.Lock, error) {
	locks := make(map[docset.Set]docset.Lock, len(sets))
	const columns = 2
	err := batchRanges(len(sets), columns, func(start, end int) error {
		args := make([]any, 0, columns*(end-start))
		for _, g := range sets[start:end] {
			args = append(args, g.Namespace, g.Key)
		}
		rows, err := q.QueryContext(ctx, `SELECT namespace, key_primary, version, claimant, at_ms, num_docs FROM doc_locks
			WHERE (namespace, key_primary) IN (VALUES `+rowPlaceholders(end-start, columns)+")", args...)
		if err != nil {
			return fmt.Errorf("load doc locks: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var g docset.Set
			var l docset.Lock
			var at int64
			if err := rows.Scan(&g.Namespace, &g.Key, &l.Version, &l.Claimant, &at, &l.NumDocs); err != nil {
				return fmt.Errorf("scan doc lock: %w", err)
			}
			l.At = time.UnixMilli(at).UTC()
			locks[g] = l
		}
		return rows.Err()
	})
	return locks, err
}

// lockOf returns g's lock from locks, or docset.Absent.
func lockOf(locks map[docset.Set]docset.Lock, g docset.Set) docset.Lock {
	if l, ok := locks[g]; ok {
		return l
	}
	return docset.Absent
}

// saveDocLocks writes locks, creating or replacing each set's.
func saveDocLocks(ctx context.Context, tx *sql.Tx, locks map[docset.Set]docset.Lock) error {
	sets := make([]docset.Set, 0, len(locks))
	for g := range locks {
		sets = append(sets, g)
	}
	const columns = 6
	return batchRanges(len(sets), columns, func(start, end int) error {
		args := make([]any, 0, columns*(end-start))
		for _, g := range sets[start:end] {
			l := locks[g]
			args = append(args, g.Namespace, g.Key, l.Version, l.Claimant, l.At.UnixMilli(), l.NumDocs)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO doc_locks (namespace, key_primary, version, claimant, at_ms, num_docs)
			VALUES `+rowPlaceholders(end-start, columns)+`
			ON CONFLICT (namespace, key_primary) DO UPDATE SET
				version = excluded.version, claimant = excluded.claimant, at_ms = excluded.at_ms,
				num_docs = excluded.num_docs`, args...); err != nil {
			return fmt.Errorf("save doc locks: %w", err)
		}
		return nil
	})
}

// storedAt is the arrival time a write stores. A write names its arrival only
// as a duration from this backend's own now (backend Modify contract); the cap
// keeps a negative one from ordering the task ahead of everything already
// waiting. The result is cut to the millisecond the database keeps, so
// comparing it with now, which is kept the same way, gives the answer a later
// read will, and the response carries what was stored.
func storedAt(by time.Duration, now time.Time) time.Time {
	return time.UnixMilli(entroq.NormalizeArrival(now.Add(by), now).UnixMilli()).UTC()
}
