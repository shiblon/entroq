package eqsqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/shiblon/entroq"
)

// ClaimDocs claims all docs with the requested primary key in a namespace.
// It returns a DependencyError if another claimant holds any matching doc.
func (b *EQSQLite) ClaimDocs(ctx context.Context, q *entroq.DocClaim) ([]*entroq.Doc, error) {
	if q == nil {
		return nil, fmt.Errorf("eqsqlite claim docs: nil query")
	}
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("eqsqlite claim docs: %w", err)
	}
	value, err := b.write(ctx, func(ctx context.Context, tx *sql.Tx) (any, error) {
		now := nowUTC()
		rows, err := tx.QueryContext(ctx, "SELECT "+docColumns+` FROM docs
                    WHERE namespace = ? AND key_primary = ?
                    ORDER BY key_secondary, id`, q.Namespace, q.Key)
		if err != nil {
			return nil, err
		}
		var docs []*entroq.Doc
		for rows.Next() {
			doc, err := scanDoc(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			docs = append(docs, doc)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		depErr := &entroq.DependencyError{}
		for _, doc := range docs {
			if heldDoc(doc, q.Claimant, now) {
				depErr.DocClaims = append(depErr.DocClaims, doc.IDVersion())
			}
		}
		if depErr.HasAny() {
			return nil, depErr
		}
		at := now.Add(q.Duration)
		for _, doc := range docs {
			doc.Version++
			doc.Claimant = q.Claimant
			doc.At = at
			doc.Modified = now
		}
		if len(docs) > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE docs
					SET version = version + 1, claimant = ?, at_ms = ?, modified_ms = ?
					WHERE namespace = ? AND key_primary = ?`,
				q.Claimant, at.UnixMilli(), now.UnixMilli(), q.Namespace, q.Key); err != nil {
				return nil, err
			}
		}
		return docs, nil
	})
	if err != nil {
		return nil, fmt.Errorf("eqsqlite claim docs: %w", err)
	}
	return value.([]*entroq.Doc), nil
}
