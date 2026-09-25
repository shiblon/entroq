package eqmem

import (
	"fmt"

	"github.com/google/btree"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/docgroup"
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
	// locks holds each doc group's lock, by primary key. A group can have a
	// lock and no docs (a claimed empty group), so locks is kept apart from
	// the docs rather than on them.
	locks *btree.BTreeG[lockEntry]
}

// lockEntry is a doc group's lock in a namespace.
type lockEntry struct {
	Key  string
	Lock docgroup.Lock
}

func lockKeyLess(a, b lockEntry) bool {
	return a.Key < b.Key
}

const btreeDegree = 32

func newDocNamespace(name string) *docNamespace {
	return &docNamespace{
		name:  name,
		byID:  make(map[string]*entroq.Doc),
		byKey: btree.NewG(btreeDegree, docKeyLess),
		locks: btree.NewG(btreeDegree, lockKeyLess),
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

// Len counts the namespace's docs and locks, so a namespace holding only a
// claimed empty group is kept.
func (s *docNamespace) Len() int {
	if s == nil {
		return 0
	}
	return len(s.byID) + s.locks.Len()
}

// Lock returns the lock of the group with the given primary key, or
// docgroup.Absent.
func (s *docNamespace) Lock(key string) docgroup.Lock {
	return lockIn(s.locks, key)
}

// lockIn returns key's lock in locks, or docgroup.Absent. It works on a
// snapshot as well as on the live tree.
func lockIn(locks *btree.BTreeG[lockEntry], key string) docgroup.Lock {
	if e, ok := locks.Get(lockEntry{Key: key}); ok {
		return e.Lock
	}
	return docgroup.Absent
}

// SetLock records the lock of the group with the given primary key.
func (s *docNamespace) SetLock(key string, l docgroup.Lock) {
	s.locks.ReplaceOrInsert(lockEntry{Key: key, Lock: l})
}

// DeleteLock removes the lock of the group with the given primary key.
func (s *docNamespace) DeleteLock(key string) {
	s.locks.Delete(lockEntry{Key: key})
}

// Members returns the docs of the group with the given primary key, in
// secondary key order.
func (s *docNamespace) Members(key string) []*entroq.Doc {
	var docs []*entroq.Doc
	s.AscendFrom(docKeyEntry{Key: key}, func(d *entroq.Doc) bool {
		if d.Key != key {
			return false
		}
		docs = append(docs, d)
		return true
	})
	return docs
}

func (s *docNamespace) Get(id string) (*entroq.Doc, bool) {
	d, ok := s.byID[id]
	return d, ok
}

// snapshot returns clones of the doc and lock trees for lock-free range
// scanning. Must be called with the namespace lock held; the returned trees may
// be iterated after the lock is released. Cloning is copy-on-write, so it is
// cheap.
func (s *docNamespace) snapshot() (*btree.BTreeG[docKeyEntry], *btree.BTreeG[lockEntry]) {
	return s.byKey.Clone(), s.locks.Clone()
}

// AscendFrom iterates live docs whose entry is >= pivot, in (key, secondary, id) order.
// Must be called with the namespace lock held.
func (s *docNamespace) AscendFrom(pivot docKeyEntry, f func(*entroq.Doc) bool) {
	s.byKey.AscendGreaterOrEqual(pivot, func(e docKeyEntry) bool {
		return f(e.Doc)
	})
}
