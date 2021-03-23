// Package mem is an in-memory implementation of an EntroQ backend.
package mem // import "entrogo.com/entroq/mem"

import (
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

// Opener returns a constructor of the in-memory backend.
func Opener() entroq.BackendOpener {
	return func(ctx context.Context) (entroq.Backend, error) {
		return New(ctx), nil
	}
}

// TODO:
// - Have a read-only snapshot that uses an RLock for snapshotting itself and for use.
// - Have working memory that is read-write, and represents a diff from the snapshot and the current state of things.
// - A goroutine (?) that moves things from the working memory into the snapshot, using a WLock on it and an RLock on working memory.
// - All modifications RLock the snapshot and WLock working memory to compute the next working memory state.
// - Some statistics (queue length, max claims, max attempts, for example) can be updated as we go.
// - WaitEmpty can be implemented in that case.
// - BIG QUESTIONS about hwo to make statistics less computationally intensive.

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
		h = newHeap(t.Queue)
		b.heaps[t.Queue] = h
	}
	item := newItem(t)
	h.PushItem(item)
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
	h.RemoveItem(item)
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
		b.heaps[item.task.Queue].FixItem(item)
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

// QueueStats returns a map of queue names to queue statistics.
func (b *backend) QueueStats(ctx context.Context, qq *entroq.QueuesQuery) (map[string]*entroq.QueueStat, error) {
	defer un(lock(b))

	now, err := b.Time(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "tasks current time")
	}

	qs := make(map[string]*entroq.QueueStat)
	for q, heap := range b.heaps {
		if qq.Limit > 0 && len(qs) >= qq.Limit {
			break
		}

		if len(qq.MatchPrefix) != 0 || len(qq.MatchExact) != 0 {
			if !matchesPrefix(q, qq.MatchPrefix...) && !matchesExact(q, qq.MatchExact...) {
				// no match
				continue
			}
		}
		// Find out how many tasks are claimed. This means they both have a
		// claimant and their arrival time is in the future.
		claimed := 0
		available := 0
		maxClaims := 0
		for _, item := range heap.Items() {
			if item.task.At.After(now) {
				if item.task.Claimant != uuid.Nil {
					claimed++
				}
			} else {
				available++
			}
			if int(item.task.Claims) > maxClaims {
				maxClaims = int(item.task.Claims)
			}
		}
		qs[q] = &entroq.QueueStat{
			Name:      q,
			Size:      heap.Len(),
			Claimed:   claimed,
			Available: available,
			MaxClaims: maxClaims,
		}
	}
	return qs, nil
}

// Tasks gets a listing of tasks in a given queue for a given (possibly empty, for all tasks) claimant.
func (b *backend) Tasks(ctx context.Context, tq *entroq.TasksQuery) ([]*entroq.Task, error) {
	defer un(lock(b))

	now, err := b.Time(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "tasks current time")
	}

	var items []*hItem

	if len(tq.IDs) != 0 {
		// Work by the (likely smaller) ID list, instead of by everything in the queue.
		for _, id := range tq.IDs {
			item, ok := b.byID[id]
			if !ok {
				continue
			}
			if tq.Queue != "" && tq.Queue != item.task.Queue {
				continue
			}
			items = append(items, item)
		}
	} else if tq.Queue != "" {
		// Queue specified with no ID filter, get everything.
		items = b.heaps[tq.Queue].Items()
	}

	// Nothing passed, exit early
	if len(items) == 0 {
		return nil, nil
	}

	var tasks []*entroq.Task

	// By this point, we have already filtered on the ID list if there is one.
	// Just apply claimant and limit filters now.
	for _, item := range items {
		if tq.Limit > 0 && len(tasks) >= tq.Limit {
			break
		}
		t := item.task
		if tq.Claimant == uuid.Nil || now.After(t.At) || tq.Claimant == t.Claimant {
			if tq.OmitValues {
				tasks = append(tasks, t.CopyOmitValue())
			} else {
				tasks = append(tasks, t.Copy())
			}
		}
	}
	return tasks, nil
}

// Claim attempts to claim a task from the queue, blocking until one is
// available or the operation is canceled.
func (b *backend) Claim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	return entroq.WaitTryClaim(ctx, cq, b.TryClaim, b.nw)
}

func (b *backend) matchClaimHeapsUnsafe(cq *entroq.ClaimQuery) []*taskHeap {
	var heaps []*taskHeap
	for _, q := range cq.Queues {
		if h, ok := b.heaps[q]; ok {
			heaps = append(heaps, h)
		}
	}
	return heaps
}

func (b *backend) tryClaimHeapUnsafe(ctx context.Context, cq *entroq.ClaimQuery, h *taskHeap) (*entroq.Task, error) {
	if h.Len() == 0 {
		return nil, nil
	}

	now, err := b.Time(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "try claim current time")
	}
	top := h.items[0].task
	if top.At.After(now) {
		return nil, nil
	}

	// Found one.
	top = top.Copy()
	top.Version++
	top.Claims++
	top.At = now.Add(cq.Duration)
	top.Modified = now
	top.Claimant = cq.Claimant
	h.items[0].task = top

	h.FixItem(h.items[0])

	return top, nil
}

// TryClaim attempts to claim against multiple queues. Only one will win.
func (b *backend) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	defer un(lock(b))

	heaps := b.matchClaimHeapsUnsafe(cq)
	rand.Shuffle(len(heaps), func(i, j int) { heaps[i], heaps[j] = heaps[j], heaps[i] })

	for _, h := range heaps {
		task, err := b.tryClaimHeapUnsafe(ctx, cq, h)
		if err != nil {
			return nil, fmt.Errorf("try claim: %w", err)
		}
		if task != nil {
			return task, nil
		}
	}
	// None found in any queue.
	return nil, nil
}

// Modify attempts to modify a batch of tasks in the queue system.
func (b *backend) Modify(ctx context.Context, mod *entroq.Modification) (inserted []*entroq.Task, changed []*entroq.Task, err error) {
	// TODO: If any of these lack a queue, fail. Queues are required, now.
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
			Attempt:  t.Attempt,
			Err:      t.Err,
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
