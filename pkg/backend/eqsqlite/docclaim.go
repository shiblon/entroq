package eqsqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/docgroup"
	"github.com/shiblon/entroq/pkg/backend/internal/validate"
)

// ClaimDocs claims the group of docs sharing the requested primary key in a
// namespace and returns its members, which may be none: a group can be claimed
// before it has docs. It returns a DependencyError listing the members while
// someone else holds the group.
func (b *EQSQLite) ClaimDocs(ctx context.Context, q *entroq.DocClaim) ([]*entroq.Doc, error) {
	if q == nil {
		return nil, fmt.Errorf("eqsqlite claim docs: nil query")
	}
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("eqsqlite claim docs: %w", err)
	}
	if err := validate.DocClaim(q); err != nil {
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

		g := docgroup.Group{Namespace: q.Namespace, Key: q.Key}
		locks, err := loadDocLocks(ctx, tx, []docgroup.Group{g})
		if err != nil {
			return nil, err
		}
		current := lockOf(locks, g)
		claimed, ok := docgroup.Claim(current, q.Claimant, now, q.Duration)
		if !ok {
			depErr := &entroq.DependencyError{}
			for _, doc := range docs {
				depErr.DocClaims = append(depErr.DocClaims, entroq.NewDocID(doc.Namespace, doc.ID, current.Version))
			}
			return nil, depErr
		}
		if err := saveDocLocks(ctx, tx, map[docgroup.Group]docgroup.Lock{g: claimed}); err != nil {
			return nil, err
		}
		for i, doc := range docs {
			docs[i] = docgroup.Overlay(doc, claimed)
		}
		return docs, nil
	})
	if err != nil {
		return nil, fmt.Errorf("eqsqlite claim docs: %w", err)
	}
	return value.([]*entroq.Doc), nil
}
