package eqsqlite

import (
	"context"
	"fmt"
	"strings"

	"github.com/shiblon/entroq"
)

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func appendMatch(where []string, args []any, column string, q *entroq.MatchQuery) ([]string, []any) {
	if q == nil || len(q.MatchExact)+len(q.MatchPrefix) == 0 {
		return where, args
	}
	parts := make([]string, 0, 2)
	if len(q.MatchExact) > 0 {
		parts = append(parts, column+" IN ("+placeholders(len(q.MatchExact))+")")
		for _, exact := range q.MatchExact {
			args = append(args, exact)
		}
	}
	for _, prefix := range q.MatchPrefix {
		parts = append(parts, "substr("+column+", 1, length(?)) = ?")
		args = append(args, prefix, prefix)
	}
	where = append(where, "("+strings.Join(parts, " OR ")+")")
	return where, args
}

// requestedOrder is an ORDER BY expression, and its arguments, that sorts rows
// by where their column's value appears in ids.
func requestedOrder(column string, ids []string) (string, []any) {
	whens := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		whens = append(whens, fmt.Sprintf("WHEN ? THEN %d", i))
		args = append(args, id)
	}
	return "CASE " + column + " " + strings.Join(whens, " ") + " ELSE " + fmt.Sprint(len(ids)) + " END", args
}

// Queues returns matching queue names and their task counts.
func (b *EQSQLite) Queues(ctx context.Context, q *entroq.QueuesQuery) (map[string]int, error) {
	return entroq.QueuesFromStats(b.QueueStats(ctx, q))
}

// QueueStats returns statistics for each matching queue.
func (b *EQSQLite) QueueStats(ctx context.Context, q *entroq.QueuesQuery) (map[string]*entroq.QueueStat, error) {
	now := nowUTC().UnixMilli()
	where, args := appendMatch(nil, nil, "queue", q)
	query := `SELECT queue, count(*),
		coalesce(sum(CASE WHEN claims > 0 AND at_ms > ? THEN 1 ELSE 0 END), 0),
        coalesce(sum(CASE WHEN at_ms <= ? THEN 1 ELSE 0 END), 0),
        coalesce(sum(CASE WHEN at_ms > ? AND claims = 0 THEN 1 ELSE 0 END), 0),
        coalesce(max(claims), 0)
        FROM tasks`
	args = append([]any{now, now, now}, args...)
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " GROUP BY queue ORDER BY queue"
	if q != nil && q.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, q.Limit)
	}
	rows, err := b.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("eqsqlite queue stats: %w", err)
	}
	defer rows.Close()
	stats := make(map[string]*entroq.QueueStat)
	for rows.Next() {
		var s entroq.QueueStat
		if err := rows.Scan(&s.Name, &s.Size, &s.Claimed, &s.Available, &s.Future, &s.MaxClaims); err != nil {
			return nil, fmt.Errorf("eqsqlite queue stats scan: %w", err)
		}
		stats[s.Name] = &s
	}
	return stats, rows.Err()
}

// Tasks returns tasks selected by the query.
func (b *EQSQLite) Tasks(ctx context.Context, q *entroq.TasksQuery) ([]*entroq.Task, error) {
	if q == nil {
		return nil, fmt.Errorf("eqsqlite tasks: nil query")
	}
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("eqsqlite tasks: %w", err)
	}
	columns := taskColumns
	if q.OmitValues {
		columns = strings.Replace(columns, "value", "NULL", 1)
	}
	var where []string
	var args []any
	if q.Queue != "" {
		where = append(where, "queue = ?")
		args = append(args, q.Queue)
	}
	if len(q.IDs) > 0 {
		where = append(where, "id IN ("+placeholders(len(q.IDs))+")")
		for _, id := range q.IDs {
			args = append(args, id)
		}
	}
	if len(where) == 0 {
		return nil, nil
	}
	if q.Claimant != "" {
		where = append(where, "(at_ms <= ? OR claimant = ?)")
		args = append(args, nowUTC().UnixMilli(), q.Claimant)
	}
	query := "SELECT " + columns + " FROM tasks WHERE " + strings.Join(where, " AND ")
	if len(q.IDs) > 0 {
		order, orderArgs := requestedOrder("id", q.IDs)
		query += " ORDER BY " + order
		args = append(args, orderArgs...)
	} else {
		query += " ORDER BY at_ms, id"
	}
	if q.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, q.Limit)
	}
	rows, err := b.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("eqsqlite tasks: %w", err)
	}
	defer rows.Close()
	var tasks []*entroq.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("eqsqlite tasks scan: %w", err)
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// Docs returns documents selected by the query.
func (b *EQSQLite) Docs(ctx context.Context, q *entroq.DocQuery) ([]*entroq.Doc, error) {
	if q == nil {
		return nil, fmt.Errorf("eqsqlite docs: nil query")
	}
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("eqsqlite docs: %w", err)
	}
	columns := docColumns
	if q.OmitValues {
		columns = strings.Replace(columns, "d.content", "NULL", 1)
	}
	where := []string{"d.namespace = ?"}
	args := []any{q.Namespace}
	switch {
	case len(q.IDs) > 0:
		where = append(where, "d.id IN ("+placeholders(len(q.IDs))+")")
		for _, id := range q.IDs {
			args = append(args, id)
		}
	case q.KeyExact != "":
		where = append(where, "d.key_primary = ?")
		args = append(args, q.KeyExact)
	default:
		if q.KeyStart != "" {
			where = append(where, "d.key_primary >= ?")
			args = append(args, q.KeyStart)
		}
		if q.KeyEnd != "" {
			where = append(where, "d.key_primary < ?")
			args = append(args, q.KeyEnd)
		}
	}
	// Docs looked up by ID come in the order asked for, ignoring any limit;
	// other docs come in key order, which a limit cuts.
	query := "SELECT " + columns + " FROM " + docsWithLocks + " WHERE " + strings.Join(where, " AND ")
	if len(q.IDs) > 0 {
		order, orderArgs := requestedOrder("d.id", q.IDs)
		query += " ORDER BY " + order
		args = append(args, orderArgs...)
	} else {
		query += " ORDER BY d.key_primary, d.key_secondary, d.id"
		if q.Limit > 0 {
			query += " LIMIT ?"
			args = append(args, q.Limit)
		}
	}
	rows, err := b.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("eqsqlite docs: %w", err)
	}
	defer rows.Close()
	var docs []*entroq.Doc
	for rows.Next() {
		doc, err := scanDoc(rows)
		if err != nil {
			return nil, fmt.Errorf("eqsqlite docs scan: %w", err)
		}
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

// NamespaceStats returns document statistics for each matching namespace.
func (b *EQSQLite) NamespaceStats(ctx context.Context, q *entroq.MatchQuery) (map[string]*entroq.NamespaceStat, error) {
	now := nowUTC().UnixMilli()
	// A doc is claimed while its group is held. Counting docs needs only the
	// docs table; counting claimed ones starts from the held locks, which are
	// few, and counts their members.
	sizeWhere, args := appendMatch([]string{"TRUE"}, nil, "d.namespace", q)
	claimedWhere, claimedArgs := appendMatch([]string{"l.claimant <> ''", "l.at_ms > ?"}, []any{now}, "l.namespace", q)
	args = append(args, claimedArgs...)
	query := `WITH sizes AS (
			SELECT d.namespace, count(*) AS size FROM docs d
			WHERE ` + strings.Join(sizeWhere, " AND ") + `
			GROUP BY d.namespace
		),
		claimed AS (
			SELECT l.namespace, count(*) AS claimed
			FROM doc_locks l
			JOIN docs d ON d.namespace = l.namespace AND d.key_primary = l.key_primary
			WHERE ` + strings.Join(claimedWhere, " AND ") + `
			GROUP BY l.namespace
		)
		SELECT s.namespace, s.size, coalesce(c.claimed, 0)
		FROM sizes s LEFT JOIN claimed c ON c.namespace = s.namespace
		ORDER BY s.namespace`
	if q != nil && q.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, q.Limit)
	}
	rows, err := b.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("eqsqlite namespace stats: %w", err)
	}
	defer rows.Close()
	stats := make(map[string]*entroq.NamespaceStat)
	for rows.Next() {
		var s entroq.NamespaceStat
		if err := rows.Scan(&s.Name, &s.Size, &s.Claimed); err != nil {
			return nil, fmt.Errorf("eqsqlite namespace stats scan: %w", err)
		}
		stats[s.Name] = &s
	}
	return stats, rows.Err()
}
