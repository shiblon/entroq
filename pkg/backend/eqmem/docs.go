package eqmem

import (
	"context"
	"fmt"
	"log"
	"slices"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/docset"
	"github.com/shiblon/entroq/pkg/backend/internal/validate"
)

// Docs returns a slice of docs in a namespace. If IDs are specified, only
// those docs are returned (key range is ignored and Limit does not apply).
// Otherwise, docs are filtered by optional key range and subject to Limit.
// Results are returned sorted by (key_primary, key_secondary). Each doc carries
// its set's version and claim.
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

	result := func(d *entroq.Doc, l docset.Lock) *entroq.Doc {
		res := docset.Overlay(d, l)
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

// ClaimDocs claims every doc set cq names, all or none (see
// docset.ClaimAll), holding every namespace involved at once, in order.
func (m *EQMem) ClaimDocs(ctx context.Context, cq *entroq.DocClaim) ([]*entroq.DocSet, error) {
	if err := validate.DocClaim(cq); err != nil {
		return nil, fmt.Errorf("eqmem claim docs: %w", err)
	}
	sets := docset.SetsOf(cq)
	var names []string
	for _, g := range sets {
		if !slices.Contains(names, g.Namespace) {
			names = append(names, g.Namespace)
		}
	}

	// Resolve the hold BEFORE taking any namespace lock, so no lock is held
	// while another is taken and this adds nothing to eqmem's lock ordering.
	//
	// The task is read optimistically and without its queue lock, which
	// docset.HoldUntil explains is sufficient: a task that changes between
	// this read and the write below leaves the hold wrong by at most one
	// lease, which the holder's next renewal corrects. The read is safe
	// because queues are a sync.Map and a writer replaces a task rather than
	// mutating it in place, so the task this returns cannot change underneath.
	now, _ := m.Time(ctx)
	until := now.Add(cq.Duration)
	if cq.TaskToMatch != nil {
		// Read without the queue lock, before any namespace lock is taken, so
		// this adds nothing to eqmem's lock ordering. The read is safe because
		// queues are a sync.Map and a writer replaces a task rather than
		// mutating it in place; see docset.TaskNotHeldErrorf for why reading it
		// optimistically is enough.
		tq := func() *taskQueue {
			defer un(lock(m))
			return m.queues[cq.TaskToMatch.Queue]
		}()
		if tq == nil {
			return nil, docset.TaskNotHeldErrorf(cq.TaskToMatch, "eqmem claim docs")
		}
		t, ok := tq.Get(cq.TaskToMatch.ID)
		if !ok || t.Version != cq.TaskToMatch.Version {
			return nil, docset.TaskNotHeldErrorf(cq.TaskToMatch, "eqmem claim docs")
		}
		until = t.At
	}

	nls, unlock := m.lockNamespaces(names)
	defer unlock()
	byNS := make(map[string]*docNamespace, len(nls))
	for _, nl := range nls {
		byNS[nl.namespace] = nl.docs
	}

	lockOf := func(g docset.Set) docset.Lock { return byNS[g.Namespace].Lock(g.Key) }
	members := func(g docset.Set) ([]*entroq.Doc, error) { return byNS[g.Namespace].Members(g.Key), nil }
	claimed, err := docset.ClaimAll(cq, now, until, lockOf, members)
	if err != nil {
		return nil, fmt.Errorf("eqmem claim docs: %w", err)
	}

	written := make(map[docset.Set]docset.Lock, len(sets))
	for i, g := range sets {
		byNS[g.Namespace].SetLock(g.Key, claimed[i])
		written[g] = claimed[i]
	}
	if err := m.journalLocks(written); err != nil {
		log.Fatalf("Inconsistent internal state: doc claim succeeded but could not be journaled: %v", err)
	}
	return docset.ClaimedSets(cq, claimed, members)
}
