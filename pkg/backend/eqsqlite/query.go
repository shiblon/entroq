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
		order := make([]string, 0, len(q.IDs))
		for i, id := range q.IDs {
			order = append(order, fmt.Sprintf("WHEN ? THEN %d", i))
			args = append(args, id)
		}
		query += " ORDER BY CASE id " + strings.Join(order, " ") + " ELSE " + fmt.Sprint(len(q.IDs)) + " END"
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
	columns := docColumns
	if q.OmitValues {
		columns = strings.Replace(columns, "content", "NULL", 1)
	}
	where := []string{"namespace = ?"}
	args := []any{q.Namespace}
	switch {
	case len(q.IDs) > 0:
		where = append(where, "id IN ("+placeholders(len(q.IDs))+")")
		for _, id := range q.IDs {
			args = append(args, id)
		}
	case q.KeyExact != "":
		where = append(where, "key_primary = ?")
		args = append(args, q.KeyExact)
	default:
		if q.KeyStart != "" {
			where = append(where, "key_primary >= ?")
			args = append(args, q.KeyStart)
		}
		if q.KeyEnd != "" {
			where = append(where, "key_primary < ?")
			args = append(args, q.KeyEnd)
		}
	}
	query := "SELECT " + columns + " FROM docs WHERE " + strings.Join(where, " AND ") + " ORDER BY key_primary, key_secondary, id"
	if q.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, q.Limit)
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
	where, args := appendMatch(nil, nil, "namespace", q)
	query := `SELECT namespace, count(*),
        coalesce(sum(CASE WHEN claimant <> '' AND at_ms > ? THEN 1 ELSE 0 END), 0)
        FROM docs`
	args = append([]any{now}, args...)
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " GROUP BY namespace ORDER BY namespace"
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
