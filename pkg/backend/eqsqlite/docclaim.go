package eqsqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/docset"
	"github.com/shiblon/entroq/pkg/backend/internal/validate"
)

// ClaimDocs claims the set of docs sharing the requested primary key in a
// namespace and returns its members, which may be none: a set can be claimed
// before it has docs. It returns a DependencyError listing the members while
// someone else holds the set.
func (b *EQSQLite) ClaimDocs(ctx context.Context, q *entroq.DocClaim) (*entroq.DocSet, error) {
	if q == nil {
		return nil, fmt.Errorf("eqsqlite claim docs: nil query")
	}
	if err := validate.DocClaim(q); err != nil {
		return nil, fmt.Errorf("eqsqlite claim docs: %w", err)
	}
	value, err := b.write(ctx, func(ctx context.Context, tx *sql.Tx) (any, error) {
		now := nowUTC()
		rows, err := tx.QueryContext(ctx, "SELECT "+docColumns+" FROM "+docsWithLocks+`
                    WHERE d.namespace = ? AND d.key_primary = ?
                    ORDER BY d.key_secondary, d.id`, q.Namespace, q.Key)
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

		g := docset.Set{Namespace: q.Namespace, Key: q.Key}
		locks, err := loadDocLocks(ctx, tx, []docset.Set{g})
		if err != nil {
			return nil, err
		}
		current := lockOf(locks, g)
		claimed, ok := docset.Claim(current, q.Claimant, now, q.Duration)
		if !ok {
			return nil, docset.HeldError(g, current, docs)
		}
		if err := saveDocLocks(ctx, tx, map[docset.Set]docset.Lock{g: claimed}); err != nil {
			return nil, err
		}
		return docset.Claimed(g, claimed, docs), nil
	})
	if err != nil {
		return nil, fmt.Errorf("eqsqlite claim docs: %w", err)
	}
	return value.(*entroq.DocSet), nil
}
