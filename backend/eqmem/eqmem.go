// Package eqmem implements an in-memory entroq that has fine-grained locking
// and can handle simultaneously stats/task listing and modifications to a
// large extent.
package eqmem

import (
	"context"
	"log"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"entrogo.com/entroq"
	"entrogo.com/entroq/subq"
	"github.com/google/uuid"
)

type EQMem struct {
	sync.Mutex

	nw entroq.NotifyWaiter

	// byQueue allows tasks to be accessed by queue name. The returned type is
	// safe for concurrent use, and follows sync.Map semantics.
	byQueue map[string]*Queue

	// byID gets the queue name for a given task ID. This is used to quickly
	// look up tasks when the queue name is unknown. That should never be the
	// case, however, since modifications are done on existing tasks, and have
	// to go through RBAC based on queue names.
	byID map[uuid.UUID]string

	// locks contains lockers for each known queue. The locks know their own
	// queue name, as well.
	locks map[string]*queueLock

	// claimIndex contains information making it easy to search for tasks to
	// claim.
	claimIndex map[string]*claimHeap
}

type queueLock struct {
	sync.Mutex
	q string
}

func lock(l sync.Locker) func() {
	l.Lock()
	return l.Unlock
}

func un(f func()) {
	f()
}

// Opener returns a constructor of the in-memory backend.
func Opener() entroq.BackendOpener {
	return func(_ context.Context) (entroq.Backend, error) {
		return New(), nil
	}
}

// New returns a new in-memory implementation, ready to be used.
func New() *EQMem {
	return &EQMem{
		nw:         subq.New(),
		byQueue:    make(map[string]*Queue),
		byID:       make(map[uuid.UUID]string),
		locks:      make(map[string]*queueLock),
		claimIndex: make(map[string]*claimHeap),
	}
}

func (m *EQMem) queueNames() []string {
	defer un(lock(m))
	var qs []string
	for q := range m.byQueue {
		qs = append(qs, q)
	}
	return qs
}

func (m *EQMem) claimLocks(cq *entroq.ClaimQuery) []*queueLock {
	defer un(lock(m))
	var locks []*queueLock
	for _, q := range cq.Queues {
		if ql, ok := m.locks[q]; ok {
			locks = append(locks, ql)
		}
	}
	// These should be shuffled, because it's for claims.
	rand.Shuffle(len(locks), func(i, j int) {
		locks[i], locks[j] = locks[j], locks[i]
	})
	return locks
}

func (m *EQMem) getTask(queue string, id uuid.UUID) (*entroq.Task, bool) {
	if queue == "" || id == uuid.Nil {
		return nil, false
	}
	tasks, ok := m.queueTasks(queue)
	if !ok {
		return nil, false
	}
	return tasks.Get(id)
}

func (m *EQMem) claimHeap(queue string) (*claimHeap, bool) {
	defer un(lock(m))
	h, ok := m.claimIndex[queue]
	return h, ok
}

func (m *EQMem) mustTryClaimOne(ql *queueLock, now time.Time, cq *entroq.ClaimQuery) *entroq.Task {
	defer un(lock(ql))
	// Has to be done inside the lock - we're trusting that holding this lock
	// means that this specific queue entry in the claim index is not being
	// messed with.
	h, ok := m.claimHeap(ql.q)
	if !ok {
		return nil
	}

	item := h.RandomAvailable(now)
	if item == nil {
		return nil
	}

	// Found one - time to modify it for claiming and return it.
	// We are under the queue lock for this task's queue, so we now have to
	// - Update the task at+claimant in the claim index.
	// - Run fix.
	// - Update the task itself in the task store.
	newAt := now.Add(cq.Duration)
	h.UpdateItem(item, newAt)

	qts, ok := m.queueTasks(ql.q)
	if !ok {
		log.Fatalf("Inconsistent internal state: could not find queue %q after finding a claimable task in it", ql.q)
	}

	var found *entroq.Task
	if err := qts.Update(item.id, func(t *entroq.Task) *entroq.Task {
		t.At = newAt
		t.Claimant = cq.Claimant
		t.Version++
		t.Claims++
		t.Modified = now

		found = t.Copy()

		return t
	}); err != nil {
		log.Fatalf("Inconsistent internal state: could not update task in %q after claim started", ql.q)
	}

	return found
}

// Claim waits for a task to be available to claim.
func (m *EQMem) Claim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	return entroq.WaitTryClaim(ctx, cq, m.TryClaim, m.nw)
}

// TryClaim attempts to claim a task from the given queue query. If no task is
// available, returns nil (not an error).
func (m *EQMem) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	// To ensure that claims, modifications, and read operations coexist
	// peacefully with minimal contention, the actual task data is in the
	// queueTasks sync.Map structure and is only edited when a lock for a
	// corresponding queue is held.
	//
	// Claim proceeds thus:
	// - Lock "everything"
	// - Obtain a slice of locks for all queues, sort by queue name.
	// - Release "everything"
	//
	// - In turn, lock a queue, then access claim index to find task to claim
	// - Update actual task in queue-task map
	// - Update the claim index arrival time and claimant
	// - Lock mod index "everything"
	// - Update the modification index arrival time and claimant
	// - Release mod index "everything"
	// - Unlock the successful queue.
	//
	// Note that because a task ID in the modification index belongs to a
	// particular queue, and changing that task requires obtaining that queue's
	// lock, it is safe to release the "everything" lock and only reacquire it
	// to update the modification index, so long as that queue's lock is held, too.

	now, err := m.Time(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "eqmem claim time")
	}

	qls := m.claimLocks(cq)
	if len(qls) == 0 {
		return nil, nil
	}

	for _, ql := range qls {
		if task := m.mustTryClaimOne(ql, now, cq); task != nil {
			return task, nil
		}
	}

	return nil, nil
}

func ensureModQueues(mod *entroq.Modification, byID map[uuid.UUID]string) {
	for _, d := range mod.Deletes {
		if d.Queue == "" {
			d.Queue = byID[d.ID]
		}
	}

	for _, d := range mod.Depends {
		if d.Queue == "" {
			d.Queue = byID[d.ID]
		}
	}

	for _, c := range mod.Changes {
		if c.FromQueue == "" {
			c.FromQueue = byID[c.ID]
		}
	}
}

type modQueue struct {
	lock  *queueLock
	tasks *Queue
	heap  *claimHeap
}

// modLocks finds all queues from a particular modification request. If any of
// the given queues are not found, then it returns a "not okay" value as the
// second parameter. Otherwise it returns a list of queue locks that can be
// locked in the caller when ready. It can create new locks (e.g., for
// insertions). The modification is altered in this call to ensure that
// everything for which a queue can be found has one (e.g., deletions that have
// only IDs will get a queue here if they can be found).
func (m *EQMem) modLocks(mod *entroq.Modification) (info []*modQueue, delNew func()) {
	// This has to be locked the whole time so that IDs and queues are matched
	// properly if queues are missing somewhere.
	defer un(lock(m))
	ensureModQueues(mod, m.byID)

	var queues map[string]bool
	lockMap := make(map[string]bool)

	var newQs []string
	var locks []*queueLock
	addLock := func(q string, addIfMissing bool) {
		// No queue, or already known, skip.
		if q == "" || lockMap[q] {
			return
		}
		ql, ok := m.locks[q]
		if !ok {
			if !addIfMissing {
				return
			}
			ql = &queueLock{q: q}
			// Just added one - keep track in case we need to remove it later.
			newQs = append(newQs, q)
			m.locks[q] = ql
		}
		lockMap[q] = true
		locks = append(locks, ql)
	}

	for _, ins := range mod.Inserts {
		addLock(ins.Queue, true)
	}
	for _, c := range mod.Changes {
		addLock(c.FromQueue, false)
		addLock(c.Queue, true)
	}
	for _, d := range mod.Deletes {
		addLock(d.Queue, false)
	}
	for _, d := range mod.Depends {
		addLock(d.Queue, false)
	}

	delNew = func() {
		if len(newQs) == 0 {
			return
		}
		// It is allowed to acquire this lock while the queue locks are
		// acquired because it is released first. Thus, the order of
		// simultaneous locks will always be queues, then global.
		defer un(lock(m))
		for _, q := range newQs {
			delete(m.locks, q)
		}
	}

	// We have all of the locks we need. Sort to avoid dining philosophers problems.
	sort.Slice(locks, func(i, j int) bool {
		return locks[i].q < locks[j].q
	})

	// Finally, fill in the full info from the qlocks, queue tasks, and heap information.
	for _, ql := range locks {
		qts, _ := m.queueTasks(ql.q)
		h := m.claimIndex[ql.q]
		info = append(info, &modQueue{
			lock:  ql,
			tasks: qts,
			heap:  h,
		})
	}

	return locks, delNew
}

// Modify attempts to do an atomic modification on the system, given a
// particular set of modification information (deletions, changes, insertions,
// dependencies).
func (m *EQMem) Modify(ctx context.Context, mod *entroq.Modification) (inserted []*entroq.Task, changed []*entroq.Task, err error) {
	// Modify does a different locking dance than Claim. Like Claim, it
	// releases the global lock quickly and leaves a gap between that and the
	// multi-queue locking that happens. Unlike Claim, it locks *all* of the
	// queue locks in a consistent order to avoid dining philosopher problems.
	// Then it assumes it has complete impunity in working with tasks in those
	// queues. Because all Modify operations do this and cannot proceed if any
	// subset of queue locks are held by another, it provides a consistent view
	// of things.
	//
	// - Find out what queues are involved in the requested modifications. If
	// 	 any are unspecified, find them first.
	//
	// - Get all relevant queue locks, order them lexicographically.
	// - Lock all queue locks, hold thme for the rest of the function (unlock at the end).
	// - Sometimes it's okay to grab the global lock before manipulating global
	//   indices (it will have already been released, and it always comes after
	//   the queue locks have been obtained).
	//
	// - Modify claimHeaps and actual tasks. Take special care with deletions and insertions.
	// - Unlock queues.

	modQueues, delNew, err := m.modLocks(mod)
	if err != nil {
		return nil, nil, errors.Wrap(err, "eqmem modify")
	}

	// Now lock all of the queues and defer unlocking them at the same time.
	byQ := make(map[string]*modQueue)
	for _, mq := range modQueues {
		defer un(lock(ql))
		byQ[mq.lock.q] = mq
	}

	defer func() {
		// When an error is returned, that means any new queue locks we created
		// need to be uncreated. This has to happen before the queue locks are
		// released, though, which is why we defer after all of the other lock
		// deferrals are in place.
		if err != nil {
			delNew()
		}
	}()

	// Find the actual tasks involved. Queues were filled in when obtaining locks.
	found := make(map[uuid.UUID]*entroq.Task)
	addFound := func(q string, id uuid.UUID) {
		t, ok := m.getTask(q, id)
		if ok {
			found[t.ID] = t
		}
	}
	for _, d := range mod.Deletes {
		addFound(d.Queue, d.ID)
	}
	for _, d := range mod.Depends {
		addFound(d.Queue, d.ID)
	}
	for _, c := range mod.Changes {
		addFound(d.FromQueue, d.ID)
	}
	for _, t := range mod.Inserts {
		addFound(d.Queue, d.ID)
	}

	if err := mod.DependencyError(found); err != nil {
		return nil, nil, errors.Wrap(err, "eqmem modify")
	}

	// Now that we know we can proceed with our process, make all of the necessary changes.
	// We got all of the queue-based stuff handed to us previously, so we
	// already hold all of the locks for that stuff and can edit with impunity.

	deleteID := func(q string, id uuid.UUID) {
		mq := byQ[q]
		mq.heap.Remove(id)
		if ok := mq.tasks.Delete(id); !ok {
			log.Fatalf("Inconsistent state: task %v not found in queue %q to delete", id, q)
		}

		defer un(lock(m))
		delete(m.byID, id)
		if mq.tasks.Len() == 0 {
			delete(m.byQueue, mq.lock.q)
			delete(m.claimIndex, m.lock.q)
			delete(m.locks, m.lock.q)
		}
	}

	insertTask := func(t *entroq.Task) {
		mq := byQ[t.Queue]
		func() {
			defer un(lock(m))
			if mq.heap == nil {
				mq.heap = newClaimHeap()
				m.claimIndex[m.lock.q] = mq.heap
			}
			if mq.tasks == nil {
				mq.tasks = NewQueue(m.lock.q)
				m.byQueue[m.lock.q] = mq.tasks
			}
		}()
		mq.heap.PushItem(newItem(m.lock.q, t.ID, t.At))
		mq.tasks.Set(t.ID, t)
	}

	updateTask := func(t *entroq.Task) {
		mq := byQ[t.Queue]
		if ok := mq.heap.UpdateID(t.ID, t.At, t.Claimant); !ok {
			log.Fatalf("Inconsistent state: task %v not found in queue heap %q", t.ID, t.Queue)
		}
		mq.tasks.Set(t.ID, t)
	}

	for _, d := range mod.Deletes {
		deleteID(d.Queue, d.ID)
	}
	for _, c := range mod.Changes {
		newTask := c.Copy()
		c.Version++
		c.Modified = now
		if c.FromQueue != c.Queue {
			deleteID(c.FromQueue, d.ID)
			insertTask(newTask)
		} else {
			// Original version was already checked earlier.
			updateTask(newTask)
		}
		changed = append(changed, newTask)
	}
	for _, t := range mod.Inserts {
		newTask := t.Copy()
		newTask.Version = 0
		newTask.Claimant = mod.Claimant
		newTask.Claims = 0
		newTask.Created = now
		newTask.Modified = now
		newTask.Attempt = 0
		newTask.Err = ""
		insertTask(newTask)
		inserted = append(inserted, newTask)
	}

	// All done!
	return inserted, changed, nil
}

// Time returns the current time.
func (m *EQMem) Time(_ context.Context) (time.Time, error) {
	return entroq.ProcessTime(), nil
}

func (m *EQMem) queueForID(id uuid.UUID) (string, bool) {
	defer un(lock(m))
	q, ok := m.byID[id]
	return q, ok
}

func (m *EQMem) queueTasks(queue string) (*Queue, bool) {
	defer un(lock(m))
	q, ok := m.byQueue[queue]
	return q, ok
}

// Tasks lists tasks according to the given query. If specific IDs are given,
// it will block for brief periods to look up corresponding queues for them.
func (m *EQMem) Tasks(ctx context.Context, tq *entroq.TasksQuery) ([]*entroq.Task, error) {
	// Short circuit if there's nothing specified.
	if tq.Queue == "" && len(tq.IDs) == 0 {
		return nil, nil
	}

	now, err := m.Time(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "eqmem tasks time")
	}

	var found []*entroq.Task
	tryAdd := func(t *entroq.Task) bool {
		if tq.Queue != "" && tq.Queue != t.Queue {
			return false
		}
		if tq.Claimant != uuid.Nil && tq.Claimant != t.Claimant && t.At.After(now) {
			return false
		}
		found = append(found, t.CopyWithValue(!t.OmitValues))
		return true
	}

	// Several cases to consider:
	// 1) IDs but no queue specified: just find all of the IDs and return their values.
	// 2) IDs and queue specified: only return IDs that match the given queue.
	// 3) No IDs specified, just a queue: iterate over the entire queue.
	//
	// If there are IDs, in other words, we find all of those and return them
	// if the queue is empty or they match.
	//
	// Otherwise we do a completely different range operation.
	if len(tq.IDs) != 0 {
		for _, id := range tq.IDs {
			q, ok := m.queueForID(id)
			if !ok || (tq.Queue != "" && tq.Queue != q) {
				continue
			}

			qts, ok := m.queueTasks(q)
			if !ok {
				continue
			}

			t, ok := qts.Get(id)
			if !ok {
				continue
			}

			if !tryAdd(t) {
				break
			}
		}
		return found, nil
	}

	// No ID list, just a queue, range over it.
	qts, ok := m.queueTasks(q)
	if !ok {
		return nil, nil
	}

	qts.Range(func(_ uuid.UUID, t *entroq.Task) bool {
		return tryAdd(t)
	})

	return found, nil
}

func queueMatches(val string, qq *entroq.QueuesQuery) bool {
	if len(qq.MatchPrefix) == 0 && len(qq.MatchExact) == 0 {
		return true
	}
	for _, p := range qq.MatchPrefix {
		if strings.HasPrefix(val, p) {
			return true
		}
	}
	for _, e := range qq.MatchExact {
		if e == val {
			return true
		}
	}
	return false
}

// Queues returns the list of queue and their sizes, based on query contents.
func (m *EQMem) Queues(ctx context.Context, qq *entroq.QueuesQuery) (map[string]int, error) {
	return entroq.QueuesFromStats(m.QueueStats(ctx, qq))
}

// QueueStats returns statistics for each queue in the query.
func (m *EQMem) QueueStats(ctx context.Context, qq *entroq.QueuesQuery) (map[string]*entroq.QueueStat, error) {
	qs := make(map[string]*entroq.QueueStat)
	for _, q := range m.queueNames() {
		if !queueMatches(q, qq) {
			continue
		}
		if qq.Limit > 0 && len(qs) > qq.Limit {
			break
		}
		qts, ok := m.queueTasks(q)
		if !ok {
			continue
		}

		stats := &entroq.QueueStat{
			Name: q,
		}
		qts.Range(func(_, t *entroq.Task) bool {
			stats.Size++
			if !now.Before(t.At) {
				if t.Claimant != uuid.Nil {
					stats.Claimed++
				}
			} else {
				stats.Available++
			}
			if c := int(t.Claims); c > stats.MaxClaims {
				stats.MaxClaims = c
			}
			return true
		})

		qs[q] = stats
	}

	return qs, nil
}

// Close cleans up this implementation.
func (*EQMem) Close() error {
	return nil
}
