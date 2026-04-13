package eqmem

import (
	"context"

	"github.com/shiblon/entroq"
)

// Docs returns a slice of docs in a namespace. If IDs are specified, only
// those docs are returned (key range is ignored and Limit does not apply).
// Otherwise, docs are filtered by optional key range and subject to Limit.
func (m *EQMem) Docs(ctx context.Context, rq *entroq.DocQuery) ([]*entroq.Doc, error) {
	nls, unlock := m.lockNamespaces([]string{rq.Namespace})
	defer unlock()

	if len(nls) == 0 {
		return nil, nil
	}
	nss := nls[0].docs

	if len(rq.IDs) > 0 {
		var found []*entroq.Doc
		for _, id := range rq.IDs {
			r, ok := nss.Get(id)
			if !ok {
				continue
			}
			res := r.Copy()
			if rq.OmitValues {
				res.Content = nil
			}
			found = append(found, res)
		}
		return found, nil
	}

	var found []*entroq.Doc
	nss.Range(func(id string, r *entroq.Doc) bool {
		if rq.KeyStart != "" && r.Key < rq.KeyStart {
			return true
		}
		if rq.KeyEnd != "" && r.Key >= rq.KeyEnd {
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

// ClaimDocs attempts an all-or-nothing claim of all docs sharing the given
// primary key in the namespace. If any doc with that key is already claimed by
// another claimant, a DependencyError is returned. Returns an empty slice (not
// an error) if no docs with the key exist.
func (m *EQMem) ClaimDocs(ctx context.Context, cq *entroq.DocClaim) ([]*entroq.Doc, error) {
	nls, unlock := m.lockNamespaces([]string{cq.Namespace})
	defer unlock()

	if len(nls) == 0 {
		return nil, nil
	}
	nss := nls[0].docs

	now, _ := m.Time(ctx)
	expiresAt := now.Add(cq.Duration)

	// Collect candidates first (no mutation during Range), then verify
	// atomically, then write.
	var candidates []*entroq.Doc
	nss.Range(func(id string, r *entroq.Doc) bool {
		if r.Key == cq.Key {
			candidates = append(candidates, r)
		}
		return true
	})

	for _, r := range candidates {
		if r.Claimant != "" && now.Before(r.ExpiresAt) && r.Claimant != cq.Claimant {
			return nil, entroq.DependencyErrorf("doc %s already claimed by %s until %v", r.ID, r.Claimant, r.ExpiresAt)
		}
	}

	var results []*entroq.Doc
	for _, r := range candidates {
		nr := r.Copy()
		nr.Claimant = cq.Claimant
		nr.ExpiresAt = expiresAt
		nr.Version++
		nr.Modified = now
		nss.Set(r.ID, nr)
		results = append(results, nr)
	}

	return results, nil
}
