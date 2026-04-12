package eqmem

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/shiblon/entroq"
)

// taskQueue is a synchronized map of task IDs to task values. It uses a
// sync.Map underneath with an atomic size counter, avoiding a separate mutex
// for size tracking while keeping reads (Get, Range) fully lock-free.
//
// Callers that mutate the queue (Set, Delete, Update) must hold the
// corresponding qLock, which provides the mutual exclusion needed for
// load-then-store operations like Update.
type taskQueue struct {
	name string
	size atomic.Int64
	byID sync.Map
}

// newTaskQueue creates a new task store, allowing retrieval, deletion,
// modification, and ranging over tasks by ID.
func newTaskQueue(name string) *taskQueue {
	return &taskQueue{name: name}
}

// Set inserts or replaces the task for the given ID.
func (s *taskQueue) Set(id string, val *entroq.Task) {
	if _, loaded := s.byID.Swap(id, val); !loaded {
		s.size.Add(1)
	}
}

// Delete removes the task for the corresponding ID, if present.
func (s *taskQueue) Delete(id string) {
	if _, loaded := s.byID.LoadAndDelete(id); loaded {
		s.size.Add(-1)
	}
}

// Update loads a task and passes it to the given update function, storing the
// result. Callers must hold the qLock for this queue; that exclusivity makes
// the load-then-store safe without an additional internal lock.
func (s *taskQueue) Update(id string, f func(*entroq.Task) *entroq.Task) error {
	val, ok := s.byID.Load(id)
	if !ok {
		return fmt.Errorf("task store update: task ID %v not found", id)
	}
	s.byID.Store(id, f(val.(*entroq.Task)))
	return nil
}

// Len returns the current number of tasks in this store.
func (s *taskQueue) Len() int {
	if s == nil {
		return 0
	}
	return int(s.size.Load())
}

// Has indicates whether a given ID is in this task store.
func (s *taskQueue) Has(id string) bool {
	_, ok := s.Get(id)
	return ok
}

// Get returns the task for the given ID, or not okay if not found.
func (s *taskQueue) Get(id string) (*entroq.Task, bool) {
	t, ok := s.byID.Load(id)
	if !ok {
		return nil, false
	}
	return t.(*entroq.Task), true
}

// Range ranges over the keys and values in this task store, calling the
// provided function for each. It follows the semantics of sync.Map precisely.
func (s *taskQueue) Range(f func(string, *entroq.Task) bool) {
	s.byID.Range(func(k, v any) bool {
		return f(k.(string), v.(*entroq.Task))
	})
}
