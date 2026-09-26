package eqpg

import (
	"context"
	"fmt"
	"strings"

	"github.com/shiblon/entroq"
)

// NamespaceStats returns doc counts per namespace, optionally filtered by the
// query's prefix/exact match criteria and capped by Limit.
func (b *EQPG) NamespaceStats(ctx context.Context, qq *entroq.MatchQuery) (_ map[string]*entroq.NamespaceStat, err error) {
	defer func() { err = interrupted(ctx, err) }()
	// A doc is claimed while its group is held. Counting docs needs only the
	// docs table; counting claimed ones starts from the held locks, which are
	// few, and counts their members.
	var values []any
	var frags []string // each formats the namespace column as %[1]s
	for _, m := range qq.MatchPrefix {
		values = append(values, likePrefix(m))
		frags = append(frags, fmt.Sprintf("%%[1]s LIKE $%d ESCAPE '\\'", len(values)))
	}
	for _, m := range qq.MatchExact {
		values = append(values, m)
		frags = append(frags, fmt.Sprintf("%%[1]s = $%d", len(values)))
	}
	match := func(col string) string {
		if len(frags) == 0 {
			return "TRUE"
		}
		return fmt.Sprintf("("+strings.Join(frags, " OR ")+")", col)
	}
	q := `WITH sizes AS (
			SELECT d.namespace, count(*) AS size FROM entroq.docs d
			WHERE ` + match("d.namespace") + `
			GROUP BY d.namespace
		),
		claimed AS (
			SELECT l.namespace, count(*) AS claimed
			FROM entroq.doc_locks l
			JOIN entroq.docs d ON d.namespace = l.namespace AND d.key_primary = l.key_primary
			WHERE l.claimant <> '' AND l.at > now() AND ` + match("l.namespace") + `
			GROUP BY l.namespace
		)
		SELECT s.namespace, s.size, coalesce(c.claimed, 0)
		FROM sizes s LEFT JOIN claimed c ON c.namespace = s.namespace`

	if qq.Limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", len(values)+1)
		values = append(values, qq.Limit)
	}

	rows, err := b.DB.QueryContext(ctx, q, values...)
	if err != nil {
		return nil, fmt.Errorf("pg namespace stats: %w", err)
	}
	defer rows.Close()

	ns := make(map[string]*entroq.NamespaceStat)
	for rows.Next() {
		var (
			name    string
			size    int
			claimed int
		)
		if err := rows.Scan(&name, &size, &claimed); err != nil {
			return nil, fmt.Errorf("pg namespace stats scan: %w", err)
		}
		ns[name] = &entroq.NamespaceStat{
			Name:    name,
			Size:    size,
			Claimed: claimed,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg namespace stats iteration: %w", err)
	}
	return ns, nil
}
