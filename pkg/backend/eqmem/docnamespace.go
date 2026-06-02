package eqmem

import (
	"fmt"

	"github.com/google/btree"
	"github.com/shiblon/entroq"
)

// docKeyEntry is the btree item type, ordered by (Key, Secondary, ID).
// Doc is embedded so that snapshots are self-contained for lock-free reads.
type docKeyEntry struct {
	Key       string
	Secondary string
	ID        string
	Doc       *entroq.Doc
}

func docKeyLess(a, b docKeyEntry) bool {
	if a.Key != b.Key {
		return a.Key < b.Key
	}
	if a.Secondary != b.Secondary {
		return a.Secondary < b.Secondary
	}
	return a.ID < b.ID
}

func entryFor(d *entroq.Doc) docKeyEntry {
	return docKeyEntry{Key: d.Key, Secondary: d.SecondaryKey, ID: d.ID, Doc: d}
}

// docNamespace stores docs in two indexes:
//   - byID: plain map for O(1) ID lookups
//   - byKey: btree ordered by (key_primary, key_secondary, id) for O(log n + k)
//     range scans
//
// All mutating methods must be called with the namespace lock held.
// snapshot() clones the btree in O(1) under the lock; the clone can be
// iterated freely after the lock is released.
type docNamespace struct {
	name  string
	byID  map[string]*entroq.Doc
	byKey *btree.BTreeG[docKeyEntry]
}

const btreeDegree = 32

func newDocNamespace(name string) *docNamespace {
	return &docNamespace{
		name:  name,
		byID:  make(map[string]*entroq.Doc),
		byKey: btree.NewG(btreeDegree, docKeyLess),
	}
}

func (s *docNamespace) Set(id string, doc *entroq.Doc) {
	if old, ok := s.byID[id]; ok {
		s.byKey.Delete(entryFor(old))
	}
	s.byKey.ReplaceOrInsert(entryFor(doc))
	s.byID[id] = doc
}

func (s *docNamespace) Delete(id string) {
	if old, ok := s.byID[id]; ok {
		s.byKey.Delete(entryFor(old))
		delete(s.byID, id)
	}
}

// Update applies f to the doc with the given ID, maintaining both indexes.
// Must be called with the namespace lock held.
func (s *docNamespace) Update(id string, f func(*entroq.Doc) *entroq.Doc) error {
	old, ok := s.byID[id]
	if !ok {
		return fmt.Errorf("doc store update: doc ID %v not found", id)
	}
	updated := f(old)
	s.byKey.Delete(entryFor(old))
	s.byKey.ReplaceOrInsert(entryFor(updated))
	s.byID[id] = updated
	return nil
}

func (s *docNamespace) Len() int {
	if s == nil {
		return 0
	}
	return len(s.byID)
}

func (s *docNamespace) Get(id string) (*entroq.Doc, bool) {
	d, ok := s.byID[id]
	return d, ok
}

// snapshot returns a clone of the btree for lock-free range scanning.
// Must be called with the namespace lock held; the returned tree may be
// iterated after the lock is released.
func (s *docNamespace) snapshot() *btree.BTreeG[docKeyEntry] {
	c := s.byKey.Clone()
	return c
}

// AscendFrom iterates live docs whose entry is >= pivot, in (key, secondary, id) order.
// Must be called with the namespace lock held.
func (s *docNamespace) AscendFrom(pivot docKeyEntry, f func(*entroq.Doc) bool) {
	s.byKey.AscendGreaterOrEqual(pivot, func(e docKeyEntry) bool {
		return f(e.Doc)
	})
}
