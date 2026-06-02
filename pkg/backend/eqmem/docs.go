package eqmem

import (
	"context"

	"github.com/shiblon/entroq"
)

// Docs returns a slice of docs in a namespace. If IDs are specified, only
// those docs are returned (key range is ignored and Limit does not apply).
// Otherwise, docs are filtered by optional key range and subject to Limit.
// Results are returned sorted by (key_primary, key_secondary).
func (m *EQMem) Docs(ctx context.Context, rq *entroq.DocQuery) ([]*entroq.Doc, error) {
	nls, unlock := m.lockNamespaces([]string{rq.Namespace})

	if len(nls) == 0 {
		unlock()
		return nil, nil
	}
	nss := nls[0].docs

	if len(rq.IDs) > 0 {
		defer unlock()
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

	// Range scan: clone under lock then release so writers aren't blocked.
	snap := nss.snapshot()
	unlock()

	limit := rq.Limit
	var found []*entroq.Doc

	collect := func(r *entroq.Doc) bool {
		if limit > 0 && len(found) >= limit {
			return false
		}
		res := r.Copy()
		if rq.OmitValues {
			res.Content = nil
		}
		found = append(found, res)
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
	claimExpiry := now.Add(cq.Duration)

	var candidates []*entroq.Doc
	nss.AscendFrom(docKeyEntry{Key: cq.Key}, func(r *entroq.Doc) bool {
		if r.Key != cq.Key {
			return false
		}
		candidates = append(candidates, r)
		return true
	})

	for _, r := range candidates {
		if r.Claimant != "" && now.Before(r.At) && r.Claimant != cq.Claimant {
			return nil, entroq.DependencyErrorf("doc %s already claimed by %s until %v", r.ID, r.Claimant, r.At)
		}
	}

	var results []*entroq.Doc
	for _, r := range candidates {
		nr := r.Copy()
		nr.Claimant = cq.Claimant
		nr.At = claimExpiry
		nr.Version++
		nr.Modified = now
		nss.Set(r.ID, nr)
		results = append(results, nr)
	}

	return results, nil
}
