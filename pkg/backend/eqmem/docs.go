package eqmem

import (
	"context"
	"fmt"
	"log"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/docgroup"
	"github.com/shiblon/entroq/pkg/backend/internal/validate"
)

// Docs returns a slice of docs in a namespace. If IDs are specified, only
// those docs are returned (key range is ignored and Limit does not apply).
// Otherwise, docs are filtered by optional key range and subject to Limit.
// Results are returned sorted by (key_primary, key_secondary). Each doc carries
// its group's version and claim.
func (m *EQMem) Docs(ctx context.Context, rq *entroq.DocQuery) ([]*entroq.Doc, error) {
	if err := rq.Validate(); err != nil {
		return nil, fmt.Errorf("eqmem docs: %w", err)
	}
	nls, unlock := m.lockNamespaces([]string{rq.Namespace})

	if len(nls) == 0 {
		unlock()
		return nil, nil
	}
	nss := nls[0].docs

	result := func(d *entroq.Doc, l docgroup.Lock) *entroq.Doc {
		res := docgroup.Overlay(d, l)
		if rq.OmitValues {
			res.Content = nil
		}
		return res
	}

	if len(rq.IDs) > 0 {
		defer unlock()
		var found []*entroq.Doc
		for _, id := range rq.IDs {
			d, ok := nss.Get(id)
			if !ok {
				continue
			}
			found = append(found, result(d, nss.Lock(d.Key)))
		}
		return found, nil
	}

	// Range scan: clone under lock then release so writers aren't blocked.
	snap, locks := nss.snapshot()
	unlock()

	limit := rq.Limit
	var found []*entroq.Doc

	collect := func(d *entroq.Doc) bool {
		if limit > 0 && len(found) >= limit {
			return false
		}
		found = append(found, result(d, lockIn(locks, d.Key)))
		return true
	}

	switch {
	case rq.KeyExact != "":
		snap.AscendGreaterOrEqual(docKeyEntry{Key: rq.KeyExact}, func(e docKeyEntry) bool {
			if e.Key != rq.KeyExact {
				return false
			}
			return collect(e.Doc)
		})
	case rq.KeyEnd != "":
		snap.AscendRange(
			docKeyEntry{Key: rq.KeyStart},
			docKeyEntry{Key: rq.KeyEnd},
			func(e docKeyEntry) bool { return collect(e.Doc) },
		)
	case rq.KeyStart != "":
		snap.AscendGreaterOrEqual(docKeyEntry{Key: rq.KeyStart}, func(e docKeyEntry) bool {
			return collect(e.Doc)
		})
	default:
		snap.Ascend(func(e docKeyEntry) bool {
			return collect(e.Doc)
		})
	}

	return found, nil
}

// ClaimDocs claims the group of docs sharing the given primary key in the
// namespace and returns its members, which may be none: a group can be claimed
// before it has docs. It fails with a DependencyError listing the members while
// someone else holds the group.
func (m *EQMem) ClaimDocs(ctx context.Context, cq *entroq.DocClaim) ([]*entroq.Doc, error) {
	if err := validate.DocClaim(cq); err != nil {
		return nil, fmt.Errorf("eqmem claim docs: %w", err)
	}
	nls, unlock := m.lockNamespaces([]string{cq.Namespace})
	defer unlock()
	nss := nls[0].docs

	now, _ := m.Time(ctx)
	members := nss.Members(cq.Key)
	current := nss.Lock(cq.Key)
	claimed, ok := docgroup.Claim(current, cq.Claimant, now, cq.Duration)
	if !ok {
		depErr := &entroq.DependencyError{
			Message: fmt.Sprintf("doc group %q in namespace %q is claimed by %s until %v", cq.Key, cq.Namespace, current.Claimant, current.At),
		}
		for _, d := range members {
			depErr.DocClaims = append(depErr.DocClaims, entroq.NewDocID(d.Namespace, d.ID, current.Version))
		}
		return nil, depErr
	}

	nss.SetLock(cq.Key, claimed)
	if err := m.journalLocks(map[docgroup.Group]docgroup.Lock{{Namespace: cq.Namespace, Key: cq.Key}: claimed}); err != nil {
		log.Fatalf("Inconsistent internal state: doc claim succeeded but could not be journaled: %v", err)
	}

	results := make([]*entroq.Doc, 0, len(members))
	for _, d := range members {
		results = append(results, docgroup.Overlay(d, claimed))
	}
	return results, nil
}
