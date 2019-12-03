// Package mem is an in-memory implementation of an EntroQ backend.
package mem // import "entrogo.com/entroq/mem"

import (
	"container/heap"
	"context"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	"entrogo.com/entroq"
	"entrogo.com/entroq/subq"
	"github.com/google/uuid"
	"github.com/pkg/errors"
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

// existsIDUnsafe must be called from within a locked mutex, returns whether the given ID exists.
func (b *backend) existsIDUnsafe(id uuid.UUID) bool {
	_, ok := b.byID[id]
	return ok
}

// existsIDVersionUnsafe must be called from within a locked mutex, returns
// whether the given ID and version exists.
func (b *backend) existsIDVersionUnsafe(id *entroq.TaskID) bool {
	if item, ok := b.byID[id.ID]; ok && item.task.Version == id.Version {
		return true
	}
	return false
}

// insertUnsafe must be called from within a locked mutex. Panics when
// duplicate insertion is attempted, so tests for duplicate IDs need to be done
// in the caller.
func (b *backend) insertUnsafe(t *entroq.Task) {
	if b.existsIDUnsafe(t.ID) {
		log.Panicf("Duplicate insertion ID attempted: %v", t.ID)
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
}

// removeUnsafe must be called from within a locked mutex. Assumes that the
// item exists - existence must be checked from the caller. Panics if queues
// get out of sync (should never happen, so there's no recovering if it does).
func (b *backend) removeUnsafe(id *entroq.TaskID) {
	if !b.existsIDVersionUnsafe(id) {
		log.Panicf("Item not found for removal: %v", id)
	}
	item := b.byID[id.ID]
	if item.task.Version != id.Version {
		log.Panicf("Item removal version mismatch: wanted %q, got %q", id, item.task.IDVersion())
	}
	h, ok := b.heaps[item.task.Queue]
	if !ok {
		log.Panicf("Queues not in sync; found item to remove in queues but not index: %v", id)
	}

	delete(b.byID, id.ID)
	heap.Remove(h, item.idx)
	if h.Len() == 0 {
		delete(b.heaps, item.task.Queue)
	}
}

// replaceUnsafe must be called from within a locked mutex. If the existing
// task isn't there, panics.
func (b *backend) replaceUnsafe(id *entroq.TaskID, newTask *entroq.Task) {
	if !b.existsIDVersionUnsafe(id) {
		log.Panicf("Item not found for task replacement: %v", id)
	}
	item := b.byID[id.ID]

	// If the queues are the same, replace and fix order. No other changes.
	if item.task.Queue == newTask.Queue {
		item.task = newTask
		h := b.heaps[item.task.Queue]
		heap.Fix(h, item.idx)
		return
	}

	// Queues are not the same. We need to remove from the first queue and add
	// to the second. Note that this might not be *perfectly* efficient because
	// we unnecessarily modify the ID index twice, but it's correct and easy to
	// understand, so fixing it would be a premature optimization.
	b.removeUnsafe(item.task.IDVersion())
	b.insertUnsafe(newTask)
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

	goodIDs := make(map[uuid.UUID]bool)
	for _, id := range tq.IDs {
		goodIDs[id] = true
	}
	idAllowed := func(id uuid.UUID) bool {
		if len(tq.IDs) == 0 {
			return true
		}
		return goodIDs[id]
	}

	for i, item := range h.items {
		if tq.Limit > 0 && i >= tq.Limit {
			break
		}
		t := item.task
		if !idAllowed(t.ID) {
			continue
		}
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

// TryClaim attempts to claim against multiple queues. Only one will win.
func (b *backend) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	defer un(lock(b))

	var heaps []*taskHeap
	// Get all of the heaps and shuffle them.
	for _, q := range cq.Queues {
		heaps = append(heaps, b.heaps[q])
	}
	rand.Shuffle(len(heaps), func(i, j int) { heaps[i], heaps[j] = heaps[j], heaps[i] })

	// Try to find a claimable task in any of them.
	for _, h := range heaps {
		if h.Len() == 0 {
			continue
		}

		now := time.Now()
		top := h.items[0].task
		if top.At.After(now) {
			continue
		}

		// Found one.
		top = top.Copy()
		top.Version++
		top.Claims++
		top.At = now.Add(cq.Duration)
		top.Modified = now
		top.Claimant = cq.Claimant
		h.items[0].task = top

		heap.Fix(h, 0)

		return top, nil
	}
	// None found in any queue.
	return nil, nil
}

// Modify attempts to modify a batch of tasks in the queue system.
func (b *backend) Modify(ctx context.Context, mod *entroq.Modification) (inserted []*entroq.Task, changed []*entroq.Task, err error) {
	defer un(lock(b))

	found := make(map[uuid.UUID]*entroq.Task)
	deps, err := mod.AllDependencies()
	if err != nil {
		return nil, nil, errors.Wrap(err, "dependency problem")
	}

	for id := range deps {
		if item, ok := b.byID[id]; ok {
			found[id] = item.task
		}
	}

	if err := mod.DependencyError(found); err != nil {
		return nil, nil, errors.Wrap(err, "modify")
	}

	now := time.Now()

	for _, t := range mod.Inserts {
		id := t.ID
		if id == uuid.Nil {
			id = uuid.New()
		}
		newTask := &entroq.Task{
			ID:       id,
			Version:  0,
			Queue:    t.Queue,
			Claimant: mod.Claimant,
			Claims:   0,
			At:       t.At,
			Created:  now,
			Modified: now,
			Value:    make([]byte, len(t.Value)),
		}
		copy(newTask.Value, t.Value)
		if b.existsIDUnsafe(newTask.ID) {
			return nil, nil, errors.Errorf("cannot insert task ID %q: already exists", newTask.ID)
		}
		inserted = append(inserted, newTask)
	}

	for _, tid := range mod.Deletes {
		if !b.existsIDVersionUnsafe(tid) {
			return nil, nil, errors.Errorf("task %q not found for deletion", tid)
		}
	}

	for _, chg := range mod.Changes {
		newTask := chg.Copy()
		if !b.existsIDVersionUnsafe(chg.IDVersion()) {
			return nil, nil, errors.Errorf("task %q not found for update", chg.IDVersion())
		}
		// Don't update version here - we do it at the last moment when actually doing the update below.
		changed = append(changed, newTask)
	}

	for _, t := range inserted {
		b.insertUnsafe(t)
	}
	for _, tid := range mod.Deletes {
		b.removeUnsafe(tid)
	}
	for _, t := range changed {
		id := t.IDVersion()
		t.Version++
		b.replaceUnsafe(id, t)
	}

	entroq.NotifyModified(b.nw, inserted, changed)

	return inserted, changed, nil
}

func (b *backend) Time(_ context.Context) (time.Time, error) {
	return entroq.ProcessTime(), nil
}
