// Package mem is an in-memory implementation of an EntroQ backend.
package mem

import (
	"container/heap"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shiblon/entroq"
)

type hItem struct {
	idx  int
	task *entroq.Task
}

type taskHeap []*hItem

func (h taskHeap) Len() int {
	return len(h)
}

func (h taskHeap) Less(i, j int) bool {
	return h[i].task.At.Before(h[j].task.At)
}

func (h taskHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].idx = i
	h[j].idx = j
}

func (h *taskHeap) Push(x interface{}) {
	it := x.(*hItem)
	it.idx = len(*h)
	*h = append(*h, it)
}

func (h *taskHeap) Pop() interface{} {
	old := *h
	n := len(old)
	it := old[n-1]
	it.idx = -1
	*h = old[:n-1]
	return it
}

// Opener returns a constructor of the in-memory backend.
func Opener() entroq.BackendOpener {
	return func(ctx context.Context) (entroq.Backend, error) {
		return New(ctx), nil
	}
}

type backend struct {
	sync.Mutex

	queueItems map[string]taskHeap
	itemsByID  map[uuid.UUID]*hItem
}

func lock(mu sync.Locker) sync.Locker {
	mu.Lock()
	return mu
}

func un(mu sync.Locker) {
	mu.Unlock()
}

func idKey(id uuid.UUID, version int) string {
	return fmt.Sprintf("%s:%s", id, version)
}

// New creates a new, empty in-memory backend.
func New(ctx context.Context) *backend {
	b := &backend{
		queueItems: make(map[string]taskHeap),
		itemsByID:  make(map[uuid.UUID]*hItem),
	}

	return b
}

// insertUnsafe must be called from within a locked mutex.
func (b *backend) insertUnsafe(t *entroq.Task) error {
	item := &hItem{task: t}
	if _, ok := b.itemsByID[t.ID]; ok {
		return fmt.Errorf("duplicate task ID insertion attempt: %v", t.ID)
	}
	// If not there, put it there. We could also use append on the long-form
	// version of the heap (specifying the whole map, etc.) but this is more
	// straightforward. Just be aware that here be dragons due to the need
	// to take a pointer to the heap before manipulating it.
	h, ok := b.queueItems[t.Queue]
	if !ok {
		h = make(taskHeap, 0)
		b.queueItems[t.Queue] = h
	}
	heap.Push(&h, &hItem{task: t})
	b.itemsByID[t.ID] = item

	return nil
}

// removeUnsafe must be called from within a locked mutex.
func (b *backend) removeUnsafe(id uuid.UUID) error {
	item, ok := b.itemsByID[id]
	if !ok {
		return fmt.Errorf("item not found for removal: %v", id)
	}
	delete(b.itemsByID, id)

	h, ok := b.queueItems[item.task.Queue]
	if !ok {
		return fmt.Errorf("queues not in sync; found item to remove queues but not in index: %v", id)
	}
	heap.Remove(&h, item.idx)
	if h.Len() == 0 {
		delete(b.queueItems, item.task.Queue)
	}

	return nil
}

// replaceUnsafe must be called from within a locked mutex.
func (b *backend) replaceUnsafe(id uuid.UUID, newTask *entroq.Task) error {
	item, ok := b.itemsByID[id]
	if !ok {
		return fmt.Errorf("item not found for task replacement: %v", id)
	}

	// If the queues are the same, replace and fix order. No other changes.
	if item.task.Queue == newTask.Queue {
		item.task = newTask
		h := b.queueItems[item.task.Queue]
		heap.Fix(&h, item.idx)
		return nil
	}

	// Queues are not the same. We need to remove from the first queue and add
	// to the second. Note that this might not be *perfectly* efficient because
	// we unnecessarily modify the ID index twice, but it's correct and easy to
	// understand, so fixing it would be a premature optimization.
	b.removeUnsafe(item.task.ID)
	b.insertUnsafe(newTask)
	return nil
}

// Close stops the background goroutines and cleans up the in-memory backend.
func (b *backend) Close() error {
	return nil
}

// Queues returns a map of queue names to sizes.
func (b *backend) Queues(ctx context.Context) (map[string]int, error) {
	defer un(lock(b))

	qs := make(map[string]int)
	for q, items := range b.queueItems {
		qs[q] = items.Len()
	}
	return qs, nil
}

// Tasks gets a listing of tasks in a given queue for a given (possibly empty, for all tasks) claimant.
func (b *backend) Tasks(ctx context.Context, tq *entroq.TasksQuery) ([]*entroq.Task, error) {
	defer un(lock(b))

	now := time.Now()

	var tasks []*entroq.Task
	for _, item := range b.queueItems[tq.Queue] {
		t := item.task
		if tq.Claimant == uuid.Nil || t.At.Before(now) || tq.Claimant == t.Claimant {
			tasks = append(tasks, t)
		}
	}
	return tasks, nil
}

// TryClaim attempts to claim a task from a queue.
func (b *backend) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	defer un(lock(b))

	items := b.queueItems[cq.Queue]

	if items.Len() == 0 {
		return nil, nil
	}

	now := time.Now()
	top := items[0].task
	if now.Before(top.At) {
		return nil, nil
	}

	top.Version++
	top.At = now.Add(cq.Duration)
	top.Modified = now
	top.Claimant = cq.Claimant

	heap.Fix(&items, 0)

	return top, nil
}

// Modify attempts to modify a batch of tasks in the queue system.
func (b *backend) Modify(ctx context.Context, mod *entroq.Modification) (inserted []*entroq.Task, changed []*entroq.Task, err error) {
	defer un(lock(b))

	found := make(map[uuid.UUID]*entroq.Task)
	for id := range mod.AllDependencies() {
		if item, ok := b.itemsByID[id]; ok {
			found[id] = item.task
		}
	}

	if err := mod.DependencyError(found); err != nil {
		return nil, nil, err
	}

	now := time.Now()

	for _, t := range mod.Inserts {
		newTask := &entroq.Task{
			ID:       uuid.New(),
			Version:  0,
			Queue:    t.Queue,
			Claimant: mod.Claimant,
			At:       t.At,
			Created:  now,
			Modified: now,
			Value:    make([]byte, len(t.Value)),
		}
		copy(newTask.Value, t.Value)

		b.insertUnsafe(newTask)
		inserted = append(inserted, newTask)
	}

	for _, tid := range mod.Deletes {
		b.removeUnsafe(tid.ID)
	}

	for _, chg := range mod.Changes {
		newTask := chg.Copy()
		b.replaceUnsafe(chg.ID, newTask)
		changed = append(changed, newTask)
	}

	return inserted, changed, nil
}
