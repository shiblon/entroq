package eqpg

import (
	"context"
	"fmt"
	"strings"

	"github.com/shiblon/entroq"
)

// NamespaceStats returns doc counts per namespace, optionally filtered by the
// query's prefix/exact match criteria and capped by Limit.
func (b *EQPG) NamespaceStats(ctx context.Context, qq *entroq.MatchQuery) (map[string]*entroq.NamespaceStat, error) {
	q := `SELECT
			namespace,
			COUNT(*) AS size,
			COUNT(*) FILTER(WHERE at > NOW() AND claimant != '') AS claimed
		FROM entroq.docs`

	var values []interface{}

	if len(qq.MatchPrefix) != 0 || len(qq.MatchExact) != 0 {
		q += " WHERE"
		var frags []string
		for _, m := range qq.MatchPrefix {
			frags = append(frags, fmt.Sprintf(" namespace LIKE $%d", len(values)+1))
			values = append(values, m+"%")
		}
		for _, m := range qq.MatchExact {
			frags = append(frags, fmt.Sprintf(" namespace = $%d", len(values)+1))
			values = append(values, m)
		}
		q += strings.Join(frags, " OR ")
	}

	q += " GROUP BY namespace"

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
