package eqsqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/docset"
	"github.com/shiblon/entroq/pkg/backend/internal/validate"
)

// ClaimDocs claims every doc set q names, all or none (see
// docset.ClaimAll), in one write transaction. A set can be claimed before it
// has docs. It returns a DependencyError naming the held sets, and their
// members, while someone else holds any of them.
func (b *EQSQLite) ClaimDocs(ctx context.Context, q *entroq.DocClaim) ([]*entroq.DocSet, error) {
	if q == nil {
		return nil, fmt.Errorf("eqsqlite claim docs: nil query")
	}
	if err := validate.DocClaim(q); err != nil {
		return nil, fmt.Errorf("eqsqlite claim docs: %w", err)
	}
	value, err := b.write(ctx, func(ctx context.Context, tx *sql.Tx) (any, error) {
		now := nowUTC()
		sets := docset.SetsOf(q)
		locks, err := loadDocLocks(ctx, tx, sets)
		if err != nil {
			return nil, err
		}
		lock := func(g docset.Set) docset.Lock { return lockOf(locks, g) }
		members := func(g docset.Set) ([]*entroq.Doc, error) {
			rows, err := tx.QueryContext(ctx, "SELECT "+docColumns+" FROM "+docsWithLocks+`
                    WHERE d.namespace = ? AND d.key_primary = ?
                    ORDER BY d.key_secondary, d.id`, g.Namespace, g.Key)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var docs []*entroq.Doc
			for rows.Next() {
				doc, err := scanDoc(rows)
				if err != nil {
					return nil, err
				}
				docs = append(docs, doc)
			}
			return docs, rows.Err()
		}
		claimed, err := docset.ClaimAll(q, now, lock, members)
		if err != nil {
			return nil, err
		}
		written := make(map[docset.Set]docset.Lock, len(sets))
		for i, g := range sets {
			written[g] = claimed[i]
		}
		if err := saveDocLocks(ctx, tx, written); err != nil {
			return nil, err
		}
		return docset.ClaimedSets(q, claimed, members)
	})
	if err != nil {
		return nil, fmt.Errorf("eqsqlite claim docs: %w", err)
	}
	return value.([]*entroq.DocSet), nil
}
