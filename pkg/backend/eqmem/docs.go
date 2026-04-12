package eqmem

import (
	"context"

	"github.com/shiblon/entroq"
)

// Docs returns a slice of docs in a namespace.
func (m *EQMem) Docs(ctx context.Context, rq *entroq.DocQuery) ([]*entroq.Doc, error) {
	nls, unlock := m.lockNamespaces([]string{rq.Namespace})
	defer unlock()

	if len(nls) == 0 {
		return nil, nil
	}
	nss := nls[0].docs

	var found []*entroq.Doc
	nss.Range(func(id string, r *entroq.Doc) bool {
		// Basic range filtering by Primary Key
		if rq.KeyStart != "" && r.KeyPrimary < rq.KeyStart {
			return true
		}
		if rq.KeyEnd != "" && r.KeyPrimary >= rq.KeyEnd {
			return true
		}

		res := r.Copy()
		if rq.OmitValues {
			res.Content = nil
		}
		found = append(found, res)

		if rq.Limit > 0 && len(found) >= rq.Limit {
			return false
		}
		return true
	})

	return found, nil
}

// ClaimDocs attempts an all-or-nothing claim of docs in a namespace.
func (m *EQMem) ClaimDocs(ctx context.Context, cq *entroq.DocClaimQuery) ([]*entroq.Doc, error) {
	nls, unlock := m.lockNamespaces([]string{cq.Namespace})
	defer unlock()

	if len(nls) == 0 {
		return nil, nil
	}
	nss := nls[0].docs

	now, _ := m.Time(ctx)
	expiresAt := now.Add(cq.Duration)

	var results []*entroq.Doc

	if len(cq.IDs) > 0 {
		// Verify all are claimable first for atomicity (all or nothing)
		var toClaim []*entroq.Doc
		for _, id := range cq.IDs {
			r, ok := nss.Get(id)
			if !ok {
				return nil, entroq.DependencyErrorf("doc %s not found in namespace %s", id, cq.Namespace)
			}
			if r.Claimant != "" && now.Before(r.ExpiresAt) && r.Claimant != cq.Claimant {
				return nil, entroq.DependencyErrorf("doc %s already claimed by %s until %v", id, r.Claimant, r.ExpiresAt)
			}
			toClaim = append(toClaim, r)
		}

		for _, r := range toClaim {
			nr := r.Copy()
			nr.Claimant = cq.Claimant
			nr.ExpiresAt = expiresAt
			nr.Version++
			nr.Modified = now
			nss.Set(r.ID, nr)
			results = append(results, nr)
		}
	} else {
		// Range claim: all-or-nothing. Collect candidates first (no mutation
		// during Range), verify none are claimed by someone else, then write.
		var candidates []*entroq.Doc
		nss.Range(func(id string, r *entroq.Doc) bool {
			if cq.KeyStart != "" && r.KeyPrimary < cq.KeyStart {
				return true
			}
			if cq.KeyEnd != "" && r.KeyPrimary >= cq.KeyEnd {
				return true
			}
			candidates = append(candidates, r)
			return true
		})

		for _, r := range candidates {
			if r.Claimant != "" && now.Before(r.ExpiresAt) && r.Claimant != cq.Claimant {
				return nil, entroq.DependencyErrorf("doc %s already claimed by %s until %v", r.ID, r.Claimant, r.ExpiresAt)
			}
		}

		for _, r := range candidates {
			nr := r.Copy()
			nr.Claimant = cq.Claimant
			nr.ExpiresAt = expiresAt
			nr.Version++
			nr.Modified = now
			nss.Set(r.ID, nr)
			results = append(results, nr)
		}
	}

	return results, nil
}
