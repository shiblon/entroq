// Package mem is an in-memory implementation of an EntroQ backend.
package mem

import (
	"container/heap"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/subq"
)

type hItem struct {
	idx  int
	task *entroq.Task
}

type taskHeap struct {
	items []*hItem
}

func (h *taskHeap) Len() int {
	if h == nil {
		return 0
	}
	return len(h.items)
}

func (h *taskHeap) Less(i, j int) bool {
	return h.items[i].task.At.Before(h.items[j].task.At)
}

func (h *taskHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.items[i].idx = i
	h.items[j].idx = j
}

func (h *taskHeap) Push(x interface{}) {
	item := x.(*hItem)
	item.idx = len(h.items)
	h.items = append(h.items, item)
}

func (h *taskHeap) Pop() interface{} {
	n := len(h.items)
	item := h.items[n-1]
	item.idx = -1
	h.items = h.items[:n-1]
	return item
}

// Opener returns a constructor of the in-memory backend.
func Opener() entroq.BackendOpener {
	return func(ctx context.Context) (entroq.Backend, error) {
		return New(ctx), nil
	}
}

type backend struct {
	sync.Mutex

	nw entroq.NotifyWaiter

	heaps map[string]*taskHeap
	byID  map[uuid.UUID]*hItem
}

func lock(mu sync.Locker) sync.Locker {
	mu.Lock()
	return mu
}

func un(mu sync.Locker) {
	mu.Unlock()
}

func idKey(id uuid.UUID, version int) string {
	return fmt.Sprintf("%s:%d", id, version)
}

// New creates a new, empty in-memory backend.
func New(ctx context.Context) *backend {
	return &backend{
		heaps: make(map[string]*taskHeap),
		byID:  make(map[uuid.UUID]*hItem),
		nw:    subq.New(),
	}
}

// insertUnsafe must be called from within a locked mutex.
func (b *backend) insertUnsafe(t *entroq.Task) error {
	if _, ok := b.byID[t.ID]; ok {
		return fmt.Errorf("duplicate task ID insertion attempt: %v", t.ID)
	}
	// If not there, put it there. We could also use append on the long-form
	// version of the heap (specifying the whole map, etc.) but this is more
	// straightforward. Just be aware that here be dragons due to the need
	// to take a pointer to the heap before manipulating it.
	h, ok := b.heaps[t.Queue]
	if !ok {
		h = new(taskHeap)
		b.heaps[t.Queue] = h
	}
	item := &hItem{task: t}
	heap.Push(h, item)
	b.byID[t.ID] = item

	return nil
}

// removeUnsafe must be called from within a locked mutex.
func (b *backend) removeUnsafe(id uuid.UUID) error {
	item, ok := b.byID[id]
	if !ok {
		return fmt.Errorf("item not found for removal: %v", id)
	}
	delete(b.byID, id)

	h, ok := b.heaps[item.task.Queue]
	if !ok {
		return fmt.Errorf("queues not in sync; found item to remove queues but not in index: %v", id)
	}
	heap.Remove(h, item.idx)
	if h.Len() == 0 {
		delete(b.heaps, item.task.Queue)
	}

	return nil
}

// replaceUnsafe must be called from within a locked mutex.
func (b *backend) replaceUnsafe(id uuid.UUID, newTask *entroq.Task) error {
	item, ok := b.byID[id]
	if !ok {
		return fmt.Errorf("item not found for task replacement: %v", id)
	}

	// If the queues are the same, replace and fix order. No other changes.
	if item.task.Queue == newTask.Queue {
		item.task = newTask
		h := b.heaps[item.task.Queue]
		heap.Fix(h, item.idx)
		return nil
	}

	// Queues are not the same. We need to remove from the first queue and add
	// to the second. Note that this might not be *perfectly* efficient because
	// we unnecessarily modify the ID index twice, but it's correct and easy to
	// understand, so fixing it would be a premature optimization.
	if err := b.removeUnsafe(item.task.ID); err != nil {
		return fmt.Errorf("update queue removal: %v", err)
	}
	if err := b.insertUnsafe(newTask); err != nil {
		return fmt.Errorf("update queue insertion: %v", err)
	}

	return nil
}

// Close stops the background goroutines and cleans up the in-memory backend.
func (b *backend) Close() error {
	return nil
}

func matchesPrefix(val string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(val, p) {
			return true
		}
	}
	return false
}

func matchesExact(val string, matches ...string) bool {
	for _, m := range matches {
		if val == m {
			return true
		}
	}
	return false
}

// Queues returns a map of queue names to sizes.
func (b *backend) Queues(ctx context.Context, qq *entroq.QueuesQuery) (map[string]int, error) {
	defer un(lock(b))

	qs := make(map[string]int)
	for q, items := range b.heaps {
		if len(qq.MatchPrefix) != 0 || len(qq.MatchExact) != 0 {
			if !matchesPrefix(q, qq.MatchPrefix...) && !matchesExact(q, qq.MatchExact...) {
				// no match
				continue
			}
		}
		qs[q] = items.Len()
		if qq.Limit > 0 && len(qs) >= qq.Limit {
			break
		}
	}
	return qs, nil
}

// Tasks gets a listing of tasks in a given queue for a given (possibly empty, for all tasks) claimant.
func (b *backend) Tasks(ctx context.Context, tq *entroq.TasksQuery) ([]*entroq.Task, error) {
	defer un(lock(b))

	now := time.Now()

	var tasks []*entroq.Task
	h := b.heaps[tq.Queue]
	if h.Len() == 0 {
		return nil, nil
	}
	for i, item := range h.items {
		if tq.Limit > 0 && i >= tq.Limit {
			break
		}
		t := item.task
		if tq.Claimant == uuid.Nil || now.After(t.At) || tq.Claimant == t.Claimant {
			tasks = append(tasks, t)
		}
	}
	return tasks, nil
}

// Claim attempts to claim a task from the queue, blocking until one is
// available or the operation is canceled.
func (b *backend) Claim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	return entroq.WaitTryClaim(ctx, cq, b.TryClaim, b.nw)
}

// TryClaim attempts to claim a task from a queue.
func (b *backend) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	defer un(lock(b))

	h := b.heaps[cq.Queue]

	if h.Len() == 0 {
		return nil, nil
	}

	now := time.Now()
	top := h.items[0].task
	if top.At.After(now) {
		return nil, nil
	}

	top = top.Copy()
	top.Version++
	top.At = now.Add(cq.Duration)
	top.Modified = now
	top.Claimant = cq.Claimant
	h.items[0].task = top

	heap.Fix(h, 0)

	return top, nil
}

// Modify attempts to modify a batch of tasks in the queue system.
func (b *backend) Modify(ctx context.Context, mod *entroq.Modification) (inserted []*entroq.Task, changed []*entroq.Task, err error) {
	defer un(lock(b))

	found := make(map[uuid.UUID]*entroq.Task)
	deps, err := mod.AllDependencies()
	if err != nil {
		return nil, nil, fmt.Errorf("dependency problem: %v", err)
	}

	for id := range deps {
		if item, ok := b.byID[id]; ok {
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

	entroq.NotifyModified(b.nw, inserted, changed)

	return inserted, changed, nil
}
