package eqmem

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/shiblon/entroq"
)

// docNamespace is a synchronized map of doc IDs to doc values.
// It mirrors taskQueue: a sync.Map for lock-free reads with an atomic size
// counter. Callers that mutate (Set, Delete, Update) must hold the
// corresponding nsLock, which provides the load-then-store exclusivity needed
// for Update without an additional internal mutex.
type docNamespace struct {
	name string
	size atomic.Int64
	byID sync.Map
}

func newDocNamespace(name string) *docNamespace {
	return &docNamespace{name: name}
}

func (s *docNamespace) Set(id string, val *entroq.Doc) {
	if _, loaded := s.byID.Swap(id, val); !loaded {
		s.size.Add(1)
	}
}

func (s *docNamespace) Delete(id string) {
	if _, loaded := s.byID.LoadAndDelete(id); loaded {
		s.size.Add(-1)
	}
}

// Update loads a doc and passes it to f, storing the result. Callers
// must hold the nsLock for this namespace.
func (s *docNamespace) Update(id string, f func(*entroq.Doc) *entroq.Doc) error {
	val, ok := s.byID.Load(id)
	if !ok {
		return fmt.Errorf("doc store update: doc ID %v not found", id)
	}
	s.byID.Store(id, f(val.(*entroq.Doc)))
	return nil
}

func (s *docNamespace) Len() int {
	if s == nil {
		return 0
	}
	return int(s.size.Load())
}

func (s *docNamespace) Get(id string) (*entroq.Doc, bool) {
	t, ok := s.byID.Load(id)
	if !ok {
		return nil, false
	}
	return t.(*entroq.Doc), true
}

func (s *docNamespace) Range(f func(string, *entroq.Doc) bool) {
	s.byID.Range(func(k, v any) bool {
		return f(k.(string), v.(*entroq.Doc))
	})
}
