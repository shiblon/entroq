package eqpg

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/lib/pq"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/docgroup"
)

// docColumns reads a doc through its group's lock, joined as l to the doc d:
// the lock holds the only version, claimant, and arrival time a member has.
// A doc whose group has no lock keeps its own.
const docColumns = `d.namespace, d.id, coalesce(l.version, d.version), coalesce(l.claimant, d.claimant), coalesce(l.at, d.at),
	d.key_primary, d.key_secondary, d.value, d.created, d.modified`

// docsWithLocks joins each doc to its group's lock, as d and l.
const docsWithLocks = `entroq.docs d LEFT JOIN entroq.doc_locks l ON l.namespace = d.namespace AND l.key_primary = d.key_primary`

// lockMembers reads the stored docs mod's doc operations name, locking them.
// Rows are locked in (namespace, id) order, before any group lock, so
// concurrent modifications cannot deadlock.
func lockMembers(ctx context.Context, tx *sql.Tx, mod *entroq.Modification) (map[string]*entroq.Doc, error) {
	var ns, ids []string
	add := func(n, id string) {
		if id != "" {
			ns = append(ns, n)
			ids = append(ids, id)
		}
	}
	for _, d := range mod.DocInserts {
		add(d.Namespace, d.ID)
	}
	for _, d := range mod.DocChanges {
		add(d.Namespace, d.ID)
	}
	for _, d := range mod.DocDeletes {
		add(d.Namespace, d.ID)
	}
	for _, d := range mod.DocDepends {
		add(d.Namespace, d.ID)
	}
	members := make(map[string]*entroq.Doc, len(ids))
	if len(ids) == 0 {
		return members, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT namespace, id, version, claimant, at, key_primary, key_secondary, value, created, modified
		FROM entroq.docs
		WHERE (namespace, id) IN (SELECT * FROM unnest($1::text[], $2::text[]))
		ORDER BY namespace, id
		FOR UPDATE`, pq.Array(ns), pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("lock doc members: %w", err)
	}
	defer rows.Close()
	docs, err := scanDocRows(rows)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		members[entroq.DocKey(d.Namespace, d.ID)] = d
	}
	return members, nil
}

// lockGroups locks the lock rows of groups and returns them with the
// database's time. The lock row is the group's mutex for the length of the
// transaction, apart from any claim on it: groups in exclusive are locked FOR
// UPDATE, and the rest, which the modification only inserts into or depends
// on, FOR SHARE, so concurrent inserts into one group proceed together while a
// claim or lock collection waits for them (see docgroup.Exclusive).
//
// Rows are locked in (namespace, key) order, a run of same-mode groups per
// statement, so modifications locking the same groups cannot deadlock, with
// two exceptions left to PostgreSQL's deadlock detector, which aborts one
// transaction for modifyHandlingRetriable to retry:
//
//   - A shared group that turns out to be held by the modification's
//     claimant is written when the holder commits, upgrading its lock. Two
//     concurrent modifications by one holder inserting into its own group
//     can each wait for the other's share. Modifications belong in a
//     worker's finalization, not in concurrent goroutines, so this is rare;
//     after the retry the group is released and the insert is a plain
//     shared one. Choosing the mode from an unlocked read of the claim
//     would avoid it, but only by acting on a check another transaction
//     can invalidate.
//   - A shared group that has no row yet is created after the existing ones
//     are locked, out of order, and so can wait on another transaction
//     creating it too.
//
// A group with no lock row gets one at docgroup.Absent's version; if the
// modification fails, the row rolls back with it.
func lockGroups(ctx context.Context, tx *sql.Tx, groups []docgroup.Group, exclusive map[docgroup.Group]bool) (map[docgroup.Group]docgroup.Lock, time.Time, error) {
	locks := make(map[docgroup.Group]docgroup.Lock, len(groups))
	if len(groups) == 0 {
		now, err := txNow(ctx, tx)
		return locks, now, err
	}
	sorted := slices.SortedFunc(slices.Values(groups), func(a, b docgroup.Group) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Key, b.Key))
	})
	var now time.Time
	for len(sorted) > 0 {
		n := 1
		for n < len(sorted) && exclusive[sorted[n]] == exclusive[sorted[0]] {
			n++
		}
		lock := shareGroups
		if exclusive[sorted[0]] {
			lock = upsertGroups
		}
		var err error
		if now, err = lock(ctx, tx, sorted[:n], locks); err != nil {
			return nil, time.Time{}, err
		}
		sorted = sorted[n:]
	}
	return locks, now, nil
}

// upsertGroups locks the lock rows of groups FOR UPDATE, creating any that
// are missing, in one statement.
func upsertGroups(ctx context.Context, tx *sql.Tx, groups []docgroup.Group, locks map[docgroup.Group]docgroup.Lock) (time.Time, error) {
	ns, keys := groupArrays(groups)
	rows, err := tx.QueryContext(ctx, `INSERT INTO entroq.doc_locks AS l (namespace, key_primary, version, claimant, at)
		SELECT n, k, $3, '', now() FROM unnest($1::text[], $2::text[]) AS g(n, k)
		ORDER BY n, k
		ON CONFLICT (namespace, key_primary) DO UPDATE SET namespace = l.namespace
		RETURNING l.namespace, l.key_primary, l.version, l.claimant, l.at, now()`,
		pq.Array(ns), pq.Array(keys), docgroup.Absent.Version)
	if err != nil {
		return time.Time{}, fmt.Errorf("lock doc groups: %w", err)
	}
	return scanLocks(rows, locks)
}

// shareGroups locks the lock rows of groups FOR SHARE. A missing row is
// created, which holds it exclusively, as creating a group is a write. A row
// another transaction creates meanwhile is invisible to the first read and
// skipped by the insert, so the next pass locks it.
func shareGroups(ctx context.Context, tx *sql.Tx, groups []docgroup.Group, locks map[docgroup.Group]docgroup.Lock) (time.Time, error) {
	var now time.Time
	for len(groups) > 0 {
		ns, keys := groupArrays(groups)
		rows, err := tx.QueryContext(ctx, `SELECT namespace, key_primary, version, claimant, at, now()
			FROM entroq.doc_locks
			WHERE (namespace, key_primary) IN (SELECT * FROM unnest($1::text[], $2::text[]))
			ORDER BY namespace, key_primary
			FOR SHARE`, pq.Array(ns), pq.Array(keys))
		if err != nil {
			return time.Time{}, fmt.Errorf("share doc groups: %w", err)
		}
		if now, err = scanLocks(rows, locks); err != nil {
			return time.Time{}, err
		}
		if groups = missingGroups(groups, locks); len(groups) == 0 {
			break
		}
		ns, keys = groupArrays(groups)
		if rows, err = tx.QueryContext(ctx, `INSERT INTO entroq.doc_locks (namespace, key_primary, version, claimant, at)
			SELECT n, k, $3, '', now() FROM unnest($1::text[], $2::text[]) AS g(n, k)
			ORDER BY n, k
			ON CONFLICT (namespace, key_primary) DO NOTHING
			RETURNING namespace, key_primary, version, claimant, at, now()`,
			pq.Array(ns), pq.Array(keys), docgroup.Absent.Version); err != nil {
			return time.Time{}, fmt.Errorf("create doc groups: %w", err)
		}
		if now, err = scanLocks(rows, locks); err != nil {
			return time.Time{}, err
		}
		groups = missingGroups(groups, locks)
	}
	return now, nil
}

// missingGroups returns the groups not yet in locks.
func missingGroups(groups []docgroup.Group, locks map[docgroup.Group]docgroup.Lock) []docgroup.Group {
	return slices.DeleteFunc(groups, func(g docgroup.Group) bool {
		_, ok := locks[g]
		return ok
	})
}

// groupArrays splits groups into parallel namespace and key arrays.
func groupArrays(groups []docgroup.Group) (ns, keys []string) {
	for _, g := range groups {
		ns = append(ns, g.Namespace)
		keys = append(keys, g.Key)
	}
	return ns, keys
}

// scanLocks reads lock rows (namespace, key, version, claimant, at, now) into
// locks, closing rows, and returns the database's time. The time is zero if
// there were no rows.
func scanLocks(rows *sql.Rows, locks map[docgroup.Group]docgroup.Lock) (time.Time, error) {
	defer rows.Close()
	var now time.Time
	for rows.Next() {
		var g docgroup.Group
		var l docgroup.Lock
		if err := rows.Scan(&g.Namespace, &g.Key, &l.Version, &l.Claimant, &l.At, &now); err != nil {
			return time.Time{}, fmt.Errorf("scan doc lock: %w", err)
		}
		locks[g] = l
	}
	return now, rows.Err()
}

// saveLocks writes the new lock of each group; lockGroups created every row.
func saveLocks(ctx context.Context, tx *sql.Tx, locks map[docgroup.Group]docgroup.Lock) error {
	if len(locks) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE entroq.doc_locks l
		SET version = u.version, claimant = u.claimant, at = u.at
		FROM unnest($1::text[], $2::text[], $3::integer[], $4::text[], $5::timestamptz[]) AS u(namespace, key_primary, version, claimant, at)
		WHERE l.namespace = u.namespace AND l.key_primary = u.key_primary`, lockArrays(locks)...); err != nil {
		return fmt.Errorf("save doc locks: %w", err)
	}
	return nil
}

// modifyDocs applies mod's doc operations inside tx, by the rules in
// docgroup, adding the written docs to resp.
func modifyDocs(ctx context.Context, tx *sql.Tx, mod *entroq.Modification, resp *entroq.ModifyResponse) error {
	if len(mod.DocInserts)+len(mod.DocChanges)+len(mod.DocDeletes)+len(mod.DocDepends) == 0 {
		return nil
	}
	stored, err := lockMembers(ctx, tx, mod)
	if err != nil {
		return err
	}
	member := func(ns, id string) *entroq.Doc { return stored[entroq.DocKey(ns, id)] }
	locks, now, err := lockGroups(ctx, tx, docgroup.Groups(mod, member), docgroup.Exclusive(mod, member))
	if err != nil {
		return err
	}
	plan := docgroup.Evaluate(mod, now, member, func(g docgroup.Group) docgroup.Lock {
		if l, ok := locks[g]; ok {
			return l
		}
		return docgroup.Absent
	})
	if plan.Err != nil {
		return plan.Err
	}
	// Each written member carries its group's new lock, the only version and
	// claim a member has.
	withLock := func(d *entroq.Doc) *entroq.Doc {
		g := docgroup.Group{Namespace: d.Namespace, Key: d.Key}
		l, ok := plan.Locks[g]
		if !ok {
			l = locks[g]
		}
		return docgroup.Overlay(d, l)
	}

	for _, ins := range mod.DocInserts {
		id := ins.ID
		if id == "" {
			id = entroq.GenHex16()
		}
		resp.InsertedDocs = append(resp.InsertedDocs, withLock(&entroq.Doc{
			Namespace: ins.Namespace, ID: id, Key: ins.Key, SecondaryKey: ins.SecondaryKey,
			Content: ins.Content, Created: now, Modified: now,
		}))
	}
	for _, chg := range mod.DocChanges {
		// A change replaces content; keys and Created belong to the stored doc.
		d := stored[entroq.DocKey(chg.Namespace, chg.ID)].Copy()
		d.Content, d.Modified = chg.Content, now
		resp.ChangedDocs = append(resp.ChangedDocs, withLock(d))
	}
	return writeDocGroups(ctx, tx, mod.DocDeletes, resp.InsertedDocs, resp.ChangedDocs, plan.Locks, now)
}

// writeDocGroups applies a modification's doc writes and group locks in one
// statement. The deletes, inserts, and changes touch distinct docs, since a
// modification names each doc at most once, so the data-modifying CTEs cannot
// conflict.
//
// An insert can still collide with a doc another transaction inserted after
// lockMembers found none: inserts into a group share its lock, so nothing
// orders them. The collision is a dependency error, as if lockMembers had seen
// the doc, and the caller rolls back.
func writeDocGroups(ctx context.Context, tx *sql.Tx, deletes []*entroq.DocID, inserts, changes []*entroq.Doc, locks map[docgroup.Group]docgroup.Lock, now time.Time) error {
	delNS, delIDs, _ := resourceIDArrays(deletes)
	args := []any{pq.Array(delNS), pq.Array(delIDs)}
	args = append(args, docArrays(inserts)...)
	args = append(args, docArrays(changes)...)
	args = append(args, lockArrays(locks)...)
	args = append(args, now)
	rows, err := tx.QueryContext(ctx, `WITH
		del AS (
			DELETE FROM entroq.docs d
			USING unnest($1::text[], $2::text[]) AS x(namespace, id)
			WHERE d.namespace = x.namespace AND d.id = x.id
		),
		ins AS (
			INSERT INTO entroq.docs (namespace, id, version, claimant, at, key_primary, key_secondary, value, created, modified)
			SELECT namespace, id, version, claimant, at, key_primary, key_secondary, value::jsonb, $24, $24
			FROM unnest($3::text[], $4::text[], $5::integer[], $6::text[], $7::timestamptz[], $8::text[], $9::text[], $10::text[])
				AS r(namespace, id, version, claimant, at, key_primary, key_secondary, value)
			ON CONFLICT (namespace, id) DO NOTHING
			RETURNING namespace, id
		),
		upd AS (
			UPDATE entroq.docs d
			SET version = c.version, claimant = c.claimant, at = c.at, value = c.value::jsonb, modified = $24
			FROM unnest($11::text[], $12::text[], $13::integer[], $14::text[], $15::timestamptz[], $16::text[], $17::text[], $18::text[])
				AS c(namespace, id, version, claimant, at, key_primary, key_secondary, value)
			WHERE d.namespace = c.namespace AND d.id = c.id
		),
		lck AS (
			UPDATE entroq.doc_locks l
			SET version = u.version, claimant = u.claimant, at = u.at
			FROM unnest($19::text[], $20::text[], $21::integer[], $22::text[], $23::timestamptz[])
				AS u(namespace, key_primary, version, claimant, at)
			WHERE l.namespace = u.namespace AND l.key_primary = u.key_primary
		)
		SELECT namespace, id FROM ins`, args...)
	if err != nil {
		return fmt.Errorf("write doc groups: %w", err)
	}
	defer rows.Close()
	inserted := make(map[string]bool, len(inserts))
	for rows.Next() {
		var ns, id string
		if err := rows.Scan(&ns, &id); err != nil {
			return fmt.Errorf("scan inserted doc: %w", err)
		}
		inserted[entroq.DocKey(ns, id)] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("write doc groups: %w", err)
	}
	depErr := new(entroq.DependencyError)
	for _, d := range inserts {
		if !inserted[entroq.DocKey(d.Namespace, d.ID)] {
			depErr.DocInserts = append(depErr.DocInserts, entroq.NewDocID(d.Namespace, d.ID, 0))
		}
	}
	if depErr.HasAny() {
		return depErr
	}
	return nil
}

// docArrays splits docs into the parallel arrays writeDocGroups expands:
// namespace, id, version, claimant, at, key, secondary key, and content.
func docArrays(docs []*entroq.Doc) []any {
	var ns, ids, claimants, pks, sks []string
	var versions []int32
	var ats []time.Time
	var values []*string
	for _, d := range docs {
		ns = append(ns, d.Namespace)
		ids = append(ids, d.ID)
		versions = append(versions, d.Version)
		claimants = append(claimants, d.Claimant)
		ats = append(ats, d.At)
		pks = append(pks, d.Key)
		sks = append(sks, d.SecondaryKey)
		values = append(values, jsonTextVal(d.Content))
	}
	return []any{pq.Array(ns), pq.Array(ids), pq.Array(versions), pq.Array(claimants),
		pq.Array(ats), pq.Array(pks), pq.Array(sks), pq.Array(values)}
}

// lockArrays splits locks into parallel arrays: namespace, key, version,
// claimant, and at.
func lockArrays(locks map[docgroup.Group]docgroup.Lock) []any {
	var ns, keys, claimants []string
	var versions []int32
	var ats []time.Time
	for g, l := range locks {
		ns = append(ns, g.Namespace)
		keys = append(keys, g.Key)
		versions = append(versions, l.Version)
		claimants = append(claimants, l.Claimant)
		ats = append(ats, l.At)
	}
	return []any{pq.Array(ns), pq.Array(keys), pq.Array(versions), pq.Array(claimants), pq.Array(ats)}
}

// txNow is the database's time for tx, the clock every lock is compared to.
func txNow(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var now time.Time
	if err := tx.QueryRowContext(ctx, "SELECT now()").Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("transaction time: %w", err)
	}
	return now, nil
}

// claimDocs claims the group g inside tx and returns its members.
func claimDocs(ctx context.Context, tx *sql.Tx, cq *entroq.DocClaim) ([]*entroq.Doc, error) {
	g := docgroup.Group{Namespace: cq.Namespace, Key: cq.Key}
	locks, now, err := lockGroups(ctx, tx, []docgroup.Group{g}, map[docgroup.Group]bool{g: true})
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT namespace, id, version, claimant, at, key_primary, key_secondary, value, created, modified
		FROM entroq.docs WHERE namespace = $1 AND key_primary = $2
		ORDER BY key_secondary, id`, cq.Namespace, cq.Key)
	if err != nil {
		return nil, fmt.Errorf("read doc group: %w", err)
	}
	members, err := scanDocRows(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}

	current := locks[g]
	claimed, ok := docgroup.Claim(current, cq.Claimant, now, cq.Duration)
	if !ok {
		depErr := &entroq.DependencyError{}
		for _, d := range members {
			depErr.DocClaims = append(depErr.DocClaims, entroq.NewDocID(d.Namespace, d.ID, current.Version))
		}
		return nil, depErr
	}
	if err := saveLocks(ctx, tx, map[docgroup.Group]docgroup.Lock{g: claimed}); err != nil {
		return nil, err
	}
	for i, d := range members {
		members[i] = docgroup.Overlay(d, claimed)
	}
	return members, nil
}

// collectLocksOnce removes the locks of up to batch groups that have no docs
// and are not held, so claimed-then-abandoned groups do not accumulate.
//
// Inserts into an unheld group leave its lock unchanged, so nothing on the
// lock row shows a concurrent insert. Instead collection locks the rows FOR
// UPDATE, skipping any an insert holds shared, and only then, in a statement
// that sees every insert committed before the lock, checks the groups are
// still empty. An insert that arrives later waits, then finds the row gone
// and creates the group anew.
func (b *EQPG) collectLocksOnce(ctx context.Context, batch int) (n int, err error) {
	tx, err := b.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("eqpg collect doc locks begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
			return
		}
		if cmErr := tx.Commit(); cmErr != nil {
			n, err = 0, fmt.Errorf("eqpg collect doc locks commit: %w", cmErr)
		}
	}()
	const idle = `(l.claimant = '' OR l.at <= now())
		AND NOT EXISTS (SELECT 1 FROM entroq.docs d WHERE d.namespace = l.namespace AND d.key_primary = l.key_primary)`
	rows, err := tx.QueryContext(ctx, `SELECT l.namespace, l.key_primary FROM entroq.doc_locks l
		WHERE `+idle+`
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, batch)
	if err != nil {
		return 0, fmt.Errorf("eqpg lock idle doc locks: %w", err)
	}
	var groups []docgroup.Group
	for rows.Next() {
		var g docgroup.Group
		if err := rows.Scan(&g.Namespace, &g.Key); err != nil {
			rows.Close()
			return 0, fmt.Errorf("eqpg scan idle doc lock: %w", err)
		}
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(groups) == 0 {
		return 0, err
	}
	ns, keys := groupArrays(groups)
	res, err := tx.ExecContext(ctx, `DELETE FROM entroq.doc_locks l
		USING unnest($1::text[], $2::text[]) AS g(namespace, key_primary)
		WHERE l.namespace = g.namespace AND l.key_primary = g.key_primary AND `+idle,
		pq.Array(ns), pq.Array(keys))
	if err != nil {
		return 0, fmt.Errorf("eqpg collect doc locks: %w", err)
	}
	deleted, err := res.RowsAffected()
	return int(deleted), err
}
