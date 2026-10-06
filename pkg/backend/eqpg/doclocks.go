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
	"github.com/shiblon/entroq/pkg/backend/internal/docset"
)

// docColumns reads a doc through its set's lock, joined as l to the doc d:
// the lock holds the only version, claimant, and arrival time a member has.
const docColumns = `d.namespace, d.id, l.version, l.claimant, l.at,
	d.key_primary, d.key_secondary, d.value, d.created, d.modified`

// docsWithLocks joins each doc to its set's lock, as d and l. Every doc has
// one (docs_group_fk).
const docsWithLocks = `entroq.docs d JOIN entroq.doc_locks l ON l.namespace = d.namespace AND l.key_primary = d.key_primary`

// lockMembers reads the stored docs mod's doc operations name, locking them.
// Rows are locked in (namespace, id) order, before any set lock, so
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
	rows, err := tx.QueryContext(ctx, `SELECT `+docColumns+` FROM `+docsWithLocks+`
		WHERE (d.namespace, d.id) IN (SELECT * FROM unnest($1::text[], $2::text[]))
		ORDER BY d.namespace, d.id
		FOR UPDATE OF d`, pq.Array(ns), pq.Array(ids))
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

// occupantKey indexes a doc by the place it holds in its set.
func occupantKey(ns, key, secondary string) string {
	return ns + "\x00" + key + "\x00" + secondary
}

// lockOccupants reads the docs already holding the secondary keys mod's inserts
// want, batched the way lockMembers reads the docs mod names by ID. A set is a
// map from secondary key to doc, so an insert onto an occupied secondary key is
// a collision; see docset.Evaluate.
//
// Only inserts need this: keys are immutable after creation, so nothing else can
// move a doc onto an occupied key.
//
// Call it only while already holding the sets' locks, which is what makes the
// answer trustworthy -- see the ordering note in modifyDocs. FOR UPDATE here
// guards the rows it does find; it cannot guard a row that does not exist yet,
// which is why the lock has to come first rather than this.
func lockOccupants(ctx context.Context, tx *sql.Tx, mod *entroq.Modification) (map[string]*entroq.Doc, error) {
	var ns, keys, secondaries []string
	for _, d := range mod.DocInserts {
		ns = append(ns, d.Namespace)
		keys = append(keys, d.Key)
		secondaries = append(secondaries, d.SecondaryKey)
	}
	occupants := make(map[string]*entroq.Doc, len(ns))
	if len(ns) == 0 {
		return occupants, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+docColumns+` FROM `+docsWithLocks+`
		WHERE (d.namespace, d.key_primary, d.key_secondary) IN (SELECT * FROM unnest($1::text[], $2::text[], $3::text[]))
		ORDER BY d.namespace, d.key_primary, d.key_secondary
		FOR UPDATE OF d`, pq.Array(ns), pq.Array(keys), pq.Array(secondaries))
	if err != nil {
		return nil, fmt.Errorf("lock doc occupants: %w", err)
	}
	defer rows.Close()
	docs, err := scanDocRows(rows)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		occupants[occupantKey(d.Namespace, d.Key, d.SecondaryKey)] = d
	}
	return occupants, nil
}

// lockSets locks the lock rows of sets and returns them with the
// database's time. The lock row is the set's mutex for the length of the
// transaction, apart from any claim on it: sets in exclusive, which the
// modification writes, are locked FOR UPDATE, and the rest, which it only
// depends on, FOR SHARE, so concurrent depends proceed together while a
// writer waits for them (see docset.Exclusive).
//
// Rows are locked in (namespace, key) order, a run of same-mode sets per
// statement, so modifications locking the same sets cannot deadlock.
//
// A written set with no lock row gets one at placeholderVersion; if the
// modification fails, the row rolls back with it. A shared set always has
// one, as a depend names a stored member.
func lockSets(ctx context.Context, tx *sql.Tx, sets []docset.Set, exclusive map[docset.Set]bool) (map[docset.Set]docset.Lock, time.Time, error) {
	locks := make(map[docset.Set]docset.Lock, len(sets))
	if len(sets) == 0 {
		now, err := txNow(ctx, tx)
		return locks, now, err
	}
	sorted := slices.SortedFunc(slices.Values(sets), func(a, b docset.Set) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Key, b.Key))
	})
	var now time.Time
	for len(sorted) > 0 {
		n := 1
		for n < len(sorted) && exclusive[sorted[n]] == exclusive[sorted[0]] {
			n++
		}
		lock := shareSets
		if exclusive[sorted[0]] {
			lock = upsertSets
		}
		var err error
		if now, err = lock(ctx, tx, sorted[:n], locks); err != nil {
			return nil, time.Time{}, err
		}
		sorted = sorted[n:]
	}
	return locks, now, nil
}

// upsertSets locks the lock rows of sets FOR UPDATE, creating any that
// are missing, in one statement.
func upsertSets(ctx context.Context, tx *sql.Tx, sets []docset.Set, locks map[docset.Set]docset.Lock) (time.Time, error) {
	ns, keys := setArrays(sets)
	rows, err := tx.QueryContext(ctx, `INSERT INTO entroq.doc_locks AS l (namespace, key_primary, version, claimant, at)
		SELECT n, k, $3, '', now() FROM unnest($1::text[], $2::text[]) AS g(n, k)
		ORDER BY n, k
		ON CONFLICT (namespace, key_primary) DO UPDATE SET namespace = l.namespace
		RETURNING l.namespace, l.key_primary, l.version, l.claimant, l.at, l.num_docs, now()`,
		pq.Array(ns), pq.Array(keys), placeholderVersion)
	if err != nil {
		return time.Time{}, fmt.Errorf("lock doc sets: %w", err)
	}
	return scanLocks(rows, locks)
}

// shareSets locks the lock rows of sets FOR SHARE. They exist, as each
// holds a member the modification depends on; one that a concurrent delete
// has emptied and collection removed is simply absent, and the depend on its
// member then fails.
func shareSets(ctx context.Context, tx *sql.Tx, sets []docset.Set, locks map[docset.Set]docset.Lock) (time.Time, error) {
	ns, keys := setArrays(sets)
	rows, err := tx.QueryContext(ctx, `SELECT namespace, key_primary, version, claimant, at, num_docs, now()
		FROM entroq.doc_locks
		WHERE (namespace, key_primary) IN (SELECT * FROM unnest($1::text[], $2::text[]))
		ORDER BY namespace, key_primary
		FOR SHARE`, pq.Array(ns), pq.Array(keys))
	if err != nil {
		return time.Time{}, fmt.Errorf("share doc sets: %w", err)
	}
	return scanLocks(rows, locks)
}

// setArrays splits sets into parallel namespace and key arrays.
func setArrays(sets []docset.Set) (ns, keys []string) {
	for _, g := range sets {
		ns = append(ns, g.Namespace)
		keys = append(keys, g.Key)
	}
	return ns, keys
}

// placeholderVersion marks a lock row upsertSets created to lock a set that
// has none of its own. It is out of the range of real versions, so scanLocks
// reads such a row back as docset.Absent rather than as a set stored at a
// version -- which is what keeps an insert into that set a creation, at
// version 0, instead of a modification of something already there.
const placeholderVersion = -1

// scanLocks reads lock rows (namespace, key, version, claimant, at, num_docs,
// now) into locks, closing rows, and returns the database's time. The time is
// zero if there were no rows.
func scanLocks(rows *sql.Rows, locks map[docset.Set]docset.Lock) (time.Time, error) {
	defer rows.Close()
	var now time.Time
	for rows.Next() {
		var g docset.Set
		var l docset.Lock
		if err := rows.Scan(&g.Namespace, &g.Key, &l.Version, &l.Claimant, &l.At, &l.NumDocs, &now); err != nil {
			return time.Time{}, fmt.Errorf("scan doc lock: %w", err)
		}
		if l.Version == placeholderVersion {
			locks[g] = docset.Absent
			continue
		}
		l.Stored = true
		locks[g] = l
	}
	return now, rows.Err()
}

// saveLocks writes the new lock of each set; lockSets created every row.
func saveLocks(ctx context.Context, tx *sql.Tx, locks map[docset.Set]docset.Lock) error {
	if len(locks) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE entroq.doc_locks l
		SET version = u.version, claimant = u.claimant, at = u.at, num_docs = u.num_docs
		FROM unnest($1::text[], $2::text[], $3::integer[], $4::text[], $5::timestamptz[], $6::integer[]) AS u(namespace, key_primary, version, claimant, at, num_docs)
		WHERE l.namespace = u.namespace AND l.key_primary = u.key_primary`, lockArrays(locks)...); err != nil {
		return fmt.Errorf("save doc locks: %w", err)
	}
	return nil
}

// modifyDocs applies mod's doc operations inside tx, by the rules in
// docset, adding the written docs to resp.
func modifyDocs(ctx context.Context, tx *sql.Tx, mod *entroq.Modification, resp *entroq.ModifyResponse) error {
	if len(mod.DocInserts)+len(mod.DocChanges)+len(mod.DocDeletes)+len(mod.DocDepends)+len(mod.DocArrives) == 0 {
		return nil
	}
	stored, err := lockMembers(ctx, tx, mod)
	if err != nil {
		return err
	}
	member := func(ns, id string) *entroq.Doc { return stored[entroq.DocKey(ns, id)] }
	locks, now, err := lockSets(ctx, tx, docset.Sets(mod, member), docset.Exclusive(mod, member))
	if err != nil {
		return err
	}
	// AFTER lockSets, and it has to be. A set's lock is what serializes writers
	// to it, but FOR UPDATE cannot lock a row another transaction has not
	// inserted yet, so a probe run before the lock sees nothing and locks
	// nothing. Two inserts at one secondary key would then both find the place
	// empty: the first commits, the second takes the lock it was waiting for and
	// consults a map read before that commit. Probing while holding the lock
	// means whatever the previous writer did is already visible.
	occupants, err := lockOccupants(ctx, tx, mod)
	if err != nil {
		return err
	}
	occupant := func(g docset.Set, secondary string) *entroq.Doc {
		return occupants[occupantKey(g.Namespace, g.Key, secondary)]
	}
	plan := docset.Evaluate(mod, now, member, occupant, func(g docset.Set) docset.Lock {
		if l, ok := locks[g]; ok {
			return l
		}
		return docset.Absent
	})
	if plan.Err != nil {
		return plan.Err
	}
	// Each written member carries its set's new lock, the only version and
	// claim a member has.
	withLock := func(d *entroq.Doc) *entroq.Doc {
		g := docset.Set{Namespace: d.Namespace, Key: d.Key}
		l, ok := plan.Locks[g]
		if !ok {
			l = locks[g]
		}
		return docset.Overlay(d, l)
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
	resp.ChangedSets = plan.Arrived
	return writeDocSets(ctx, tx, mod.DocDeletes, resp.InsertedDocs, resp.ChangedDocs, plan.Locks, now)
}

// writeDocSets applies a modification's doc writes and set locks in one
// statement. The deletes, inserts, and changes touch distinct docs, since a
// modification names each doc at most once, so the data-modifying CTEs cannot
// conflict.
//
// An insert can still collide with a doc another transaction inserted after
// lockMembers found none, as members are read before their sets are locked.
// The collision is a dependency error, as if lockMembers had seen the doc, and
// the caller rolls back.
func writeDocSets(ctx context.Context, tx *sql.Tx, deletes []*entroq.DocID, inserts, changes []*entroq.Doc, locks map[docset.Set]docset.Lock, now time.Time) error {
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
			INSERT INTO entroq.docs (namespace, id, key_primary, key_secondary, value, created, modified)
			SELECT namespace, id, key_primary, key_secondary, value::jsonb, $19, $19
			FROM unnest($3::text[], $4::text[], $5::text[], $6::text[], $7::text[])
				AS r(namespace, id, key_primary, key_secondary, value)
			ON CONFLICT (namespace, id) DO NOTHING
			RETURNING namespace, id
		),
		upd AS (
			UPDATE entroq.docs d
			SET value = c.value::jsonb, modified = $19
			FROM unnest($8::text[], $9::text[], $10::text[], $11::text[], $12::text[])
				AS c(namespace, id, key_primary, key_secondary, value)
			WHERE d.namespace = c.namespace AND d.id = c.id
		),
		lck AS (
			UPDATE entroq.doc_locks l
			SET version = u.version, claimant = u.claimant, at = u.at, num_docs = u.num_docs
			FROM unnest($13::text[], $14::text[], $15::integer[], $16::text[], $17::timestamptz[], $18::integer[])
				AS u(namespace, key_primary, version, claimant, at, num_docs)
			WHERE l.namespace = u.namespace AND l.key_primary = u.key_primary
		)
		SELECT namespace, id FROM ins`, args...)
	if err != nil {
		return fmt.Errorf("write doc sets: %w", err)
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
		return fmt.Errorf("write doc sets: %w", err)
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

// docArrays splits docs into the parallel arrays writeDocSets expands:
// namespace, id, key, secondary key, and content. A doc's version and claim
// are its set's, written to the lock.
func docArrays(docs []*entroq.Doc) []any {
	var ns, ids, pks, sks []string
	var values []*string
	for _, d := range docs {
		ns = append(ns, d.Namespace)
		ids = append(ids, d.ID)
		pks = append(pks, d.Key)
		sks = append(sks, d.SecondaryKey)
		values = append(values, jsonTextVal(d.Content))
	}
	return []any{pq.Array(ns), pq.Array(ids), pq.Array(pks), pq.Array(sks), pq.Array(values)}
}

// lockArrays splits locks into parallel arrays: namespace, key, version,
// claimant, at, and doc count.
func lockArrays(locks map[docset.Set]docset.Lock) []any {
	var ns, keys, claimants []string
	var versions []int32
	var ats []time.Time
	var numDocs []int64
	for g, l := range locks {
		ns = append(ns, g.Namespace)
		keys = append(keys, g.Key)
		versions = append(versions, l.Version)
		claimants = append(claimants, l.Claimant)
		ats = append(ats, l.At)
		numDocs = append(numDocs, int64(l.NumDocs))
	}
	return []any{pq.Array(ns), pq.Array(keys), pq.Array(versions), pq.Array(claimants), pq.Array(ats), pq.Array(numDocs)}
}

// txNow is the database's time for tx, the clock every lock is compared to.
func txNow(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var now time.Time
	if err := tx.QueryRowContext(ctx, "SELECT now()").Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("transaction time: %w", err)
	}
	return now, nil
}

// claimDocs claims every set cq names inside tx, all or none (see
// docset.ClaimAll), and returns them. lockSets takes their lock rows in
// order, so claims of overlapping sets cannot deadlock.
func claimDocs(ctx context.Context, tx *sql.Tx, cq *entroq.DocClaim) ([]*entroq.DocSet, error) {
	sets := docset.SetsOf(cq)
	exclusive := make(map[docset.Set]bool, len(sets))
	for _, g := range sets {
		exclusive[g] = true
	}
	locks, now, err := lockSets(ctx, tx, sets, exclusive)
	if err != nil {
		return nil, err
	}
	lock := func(g docset.Set) docset.Lock { return locks[g] }
	members := func(g docset.Set) ([]*entroq.Doc, error) {
		rows, err := tx.QueryContext(ctx, `SELECT `+docColumns+` FROM `+docsWithLocks+`
			WHERE d.namespace = $1 AND d.key_primary = $2
			ORDER BY d.key_secondary, d.id`, g.Namespace, g.Key)
		if err != nil {
			return nil, fmt.Errorf("read doc set: %w", err)
		}
		defer rows.Close()
		return scanDocRows(rows)
	}
	// A claim matching a task reads it in this same transaction, so the hold
	// and the task's own arrival come from one reading of one clock.
	until := now.Add(cq.Duration)
	if cq.TaskToMatch != nil {
		// Read in this transaction, so the hold is the task's own stored
		// arrival. See docset.MissingTaskErrorf for why the read need not hold
		// the task still.
		if err := tx.QueryRowContext(ctx,
			`SELECT at FROM entroq.tasks WHERE id = $1 AND version = $2 AND queue = $3`,
			cq.TaskToMatch.ID, cq.TaskToMatch.Version, cq.TaskToMatch.Queue).Scan(&until); err != nil {
			return nil, docset.MissingTaskErrorf(cq.TaskToMatch, "pg claim docs")
		}
	}
	claimed, err := docset.ClaimAll(cq, now, until, lock, members)
	if err != nil {
		return nil, err
	}
	written := make(map[docset.Set]docset.Lock, len(sets))
	for i, g := range sets {
		written[g] = claimed[i]
	}
	if err := saveLocks(ctx, tx, written); err != nil {
		return nil, err
	}
	return docset.ClaimedSets(cq, claimed, members)
}

// collectLocksOnce removes the locks of up to batch sets that have no docs
// and are not held, so claimed-then-abandoned sets do not accumulate.
//
// A lock row counts its set's docs, and every write updates that count
// under the row lock, so collection only locks the rows FOR UPDATE, skipping
// any a writer holds, and checks the count again as it deletes. An insert that
// arrives later waits, then finds the row gone and creates the set anew.
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
	const idle = `(l.claimant = '' OR l.at <= now()) AND l.num_docs = 0`
	rows, err := tx.QueryContext(ctx, `SELECT l.namespace, l.key_primary FROM entroq.doc_locks l
		WHERE `+idle+`
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, batch)
	if err != nil {
		return 0, fmt.Errorf("eqpg lock idle doc locks: %w", err)
	}
	var sets []docset.Set
	for rows.Next() {
		var g docset.Set
		if err := rows.Scan(&g.Namespace, &g.Key); err != nil {
			rows.Close()
			return 0, fmt.Errorf("eqpg scan idle doc lock: %w", err)
		}
		sets = append(sets, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(sets) == 0 {
		return 0, err
	}
	ns, keys := setArrays(sets)
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
