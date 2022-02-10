package eqmem

import (
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/shiblon/entroq"
)

// taskQueue is a synchronized map of task IDs to task values. It uses a
// sync.Map underneath, but makes certain operations safer and quicker, like
// obtaining the length. It is typed, so type assertions necessary for sync.Map
// use are hidden and made simpler for this use case.
//
// It also provides a locked version of updates, allowing a load+store to be
// done atomically. Reading from the map should be as fast as a normal
// sync.Map, as it relies only on the internal locking instead of global
// locking. Mutating operations that might change the size cause global
// locking, albeit briefly. These do not affect ranges or reads.
type taskQueue struct {
	sync.Mutex // for operations that can alter the size.

	name string
	size int
	byID *sync.Map
}

// newTaskQueue creates a new task store, allowing retrieval, deletion,
// modification, and ranging over tasks by ID.
func newTaskQueue(name string) *taskQueue {
	return &taskQueue{
		name: name,
		byID: new(sync.Map),
	}
}

// Set inserts or changes a value in the store.
func (s *taskQueue) Set(id uuid.UUID, val *entroq.Task) {
	defer un(lock(s))
	if _, ok := s.byID.Load(id); !ok {
		s.size++
	}
	s.byID.Store(id, val)
}

// Delete removes the task for the corresponding ID, if present.
func (s *taskQueue) Delete(id uuid.UUID) {
	defer un(lock(s))
	if _, ok := s.byID.LoadAndDelete(id); ok {
		s.size--
	}
}

// Update loads a task and passes it to the given update function, holding the lock meanwhile.
// If the task does not exist, it will return an error and never call the given function.
func (s *taskQueue) Update(id uuid.UUID, f func(*entroq.Task) *entroq.Task) error {
	defer un(lock(s))
	val, ok := s.byID.Load(id)
	if !ok {
		return fmt.Errorf("task store update: task ID %v not found", id)
	}
	newTask := f(val.(*entroq.Task))
	s.byID.Store(id, newTask)
	return nil
}

// Len returns the current size of this task store.
func (s *taskQueue) Len() int {
	if s == nil {
		return 0
	}
	defer un(lock(s))
	return s.size
}

// Has indicates whether a given ID is in this task store.
func (s *taskQueue) Has(id uuid.UUID) bool {
	_, ok := s.Get(id)
	return ok
}

// Get returns the task for the given ID, or not okay if not found,.
func (s *taskQueue) Get(id uuid.UUID) (*entroq.Task, bool) {
	// No need to lock.
	t, ok := s.byID.Load(id)
	if !ok {
		return nil, false
	}
	return t.(*entroq.Task), true
}

// Range ranges over the keys and values in this task store, calling the
// provided function for each. It follows the semantics of sync.Map precisely.
func (s *taskQueue) Range(f func(uuid.UUID, *entroq.Task) bool) {
	// No need to lock.
	s.byID.Range(func(k, v interface{}) bool {
		return f(k.(uuid.UUID), v.(*entroq.Task))
	})
}
