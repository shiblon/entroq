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

type taskHeap []*entroq.Task

func (h taskHeap) Len() int {
	return len(h)
}

func (h taskHeap) Less(i, j int) bool {
	return h[i].At.Before(h[j].At)
}

func (h taskHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *taskHeap) Push(x interface{}) {
	*h = append(*h, x.(*entroq.Task))
}

func (h *taskHeap) Pop() interface{} {
	old := *h
	n := len(old)
	it := old[n-1]
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

	tasksByQueue map[string]taskHeap
	tasksByID    map[uuid.UUID]*entroq.Task
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
		tasksByQueue: make(map[string]taskHeap),
		tasksByID:    make(map[uuid.UUID]*entroq.Task),
	}

	return b
}

// Close stops the background goroutines and cleans up the in-memory backend.
func (b *backend) Close() error {
	return nil
}

// Queues returns a map of queue names to sizes.
func (b *backend) Queues(ctx context.Context) (map[string]int, error) {
	defer un(lock(b))

	qs := make(map[string]int)
	for q, tasks := range b.tasksByQueue {
		qs[q] = len(tasks)
	}
	return qs, nil
}

// Tasks gets a listing of tasks in a given queue for a given (possibly empty, for all tasks) claimant.
func (b *backend) Tasks(ctx context.Context, tq *entroq.TasksQuery) ([]*entroq.Task, error) {
	defer un(lock(b))

	now := time.Now()

	var tasks []*entroq.Task
	for _, t := range b.tasksByQueue[tq.Queue] {
		if tq.Claimant == uuid.Nil || t.At.Before(now) || tq.Claimant == t.Claimant {
			tasks = append(tasks, t)
		}
	}
	return tasks, nil
}

// TryClaim attempts to claim a task from a queue.
func (b *backend) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	defer un(lock(b))

	tasks := b.tasksByQueue[cq.Queue]

	if len(tasks) == 0 {
		return nil, nil
	}

	now := time.Now()
	top := tasks[0]
	if now.Before(top.At) {
		return nil, nil
	}

	top.Version++
	top.At = now.Add(cq.Duration)
	top.Modified = now
	top.Claimant = cq.Claimant

	heap.Fix(&tasks, 0)

	return top, nil
}

// Modify attempts to modify a batch of tasks in the queue system.
func (b *backend) Modify(ctx context.Context, mod *entroq.Modification) (inserted []*entroq.Task, changed []*entroq.Task, err error) {
	defer un(lock(b))

	found := make(map[uuid.UUID]*entroq.Task)
	for _, chg := range mod.Changes {
		t := b.tasksByID[chg.ID]
		if t != nil {
			found[chg.ID] = t
		}
	}
	for _, del := range mod.Deletes {
		t := b.tasksByID[del.ID]
		if t != nil {
			found[del.ID] = t
		}
	}
	for _, dep := range mod.Depends {
		t := b.tasksByID[dep.ID]
		if t != nil {
			found[dep.ID] = t
		}
	}

	if err := mod.DependencyError(found); err != nil {
		return nil, nil, err
	}

	now := time.Now()

	for _, t := range mod.Inserts {
		h := b.tasksByQueue[t.Queue]
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

		inserted = append(inserted, newTask)
		heap.Push(&h, newTask)
		b.tasksByID[newTask.ID] = newTask
	}

	for _, tid := range mod.Deletes {
		t := b.tasksByID[tid.ID]
		delete(b.tasksByID, tid.ID)
		// TODO: make more efficient.
		h := b.tasksByQueue[t.Queue]
		for i, found := range h {
			if found.ID == t.ID {
				heap.Remove(&h, i)
				break
			}
		}
	}

	for _, chg := range mod.Changes {
		newTask := chg.Copy()
		old := b.tasksByID[chg.ID]
		b.tasksByID[chg.ID] = newTask

		// TODO: make finding the index more efficient.
		h := b.tasksByQueue[old.Queue]
		for i, found := range h {
			if found.ID == chg.ID {
				if old.Queue == chg.Queue {
					*old = *newTask
					heap.Fix(&h, i)
				} else {
					heap.Remove(&h, i)
					newH := b.tasksByQueue[chg.Queue]
					heap.Push(&newH, newTask)
				}
				changed = append(changed, newTask)
				break
			}
		}
	}

	return inserted, changed, nil
}
