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
)

type EQMem struct {
	sync.Mutex

	// byQueue allows tasks to be accessed by queue name. The returned type is
	// safe for concurrent use, and follows sync.Map semantics.
	byQueue map[string]*Tasks

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

// New returns a new in-memory implementation, ready to be used.
func New() *EQMem {
	return &EQMem{
		queueTasks: new(sync.Map),
		queueLocks: make(map[string]*queueLock),
		modIndex:   makeModMap(),
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
	var locks []queueLock
	for _, q := range cq.Queues {
		if ql, ok := m.queueLocks[q]; ok {
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
	tasks, ok := m.queueTasks.Load(queue)
	if !ok {
		return nil, false
	}

	task, ok := tasks.(*sync.Map).Load(id)
	if !ok {
		return nil, false
	}

	return task.(*entroq.Task), true
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
	newAt = now.Add(cq.Duration)
	h.UpdateItem(item, newAt, cq.Claimant)

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
		if task := m.mustTryClaimOne(now, ql); task != nil {
			return task, nil
		}
	}

	return nil, nil
}

// modLocks finds all queues from a particular modification request. If any of
// the given queues are not found, then it returns a "not okay" value as the
// second parameter. Otherwise it returns a list of queue locks that can be
// locked in the caller when ready. It can create new locks (e.g., for
// insertions).
func (m *EQMem) modLocks(mod *entroq.Modification) (locks []*queueLock, delNew func(), err error) {
	// This has to be locked the whole time so that IDs and queues are matched
	// properly if queues are missing somewhere.
	defer un(lock(m))

	depErr := new(entroq.DependencyError)

	var queues map[string]bool
	lockMap = make(map[string]bool)

	addLock := func(q string, addIfMissing bool) bool {
		// Already there, skip.
		if lockMap[q] {
			return true
		}
		ql, ok := m.locks[q]
		if !ok {
			if !addIfMissing {
				return false
			}
			ql := &queueLock{q: q}
			// Just added one - keep track in case we need to remove it later.
			newQs = append(newQs, q)
			m.locks[q] = ql
		}
		lockMap[q] = true
		locks = append(locks, ql)
		return true
	}

	for _, ins := range mod.Inserts {
		addLock(ins.Queue, true)
	}

	for _, c := range mod.Changes {
		if c.FromQueue == "" {
			q, ok := m.byID[c.ID]
			if !ok {
				depErr.Changes = append(depErr.Changes, c.IDVersion())
				continue
			}
			c.FromQueue = q
		}
		if !addLock(c.FromQueue, false) {
			depErr.Changes = append(depErr.Changes, c.IDVersion())
		}
		addLock(c.Queue, true)
	}

	for _, d := range mod.Deletes {
		if d.Queue == "" {
			q, ok := m.byID[d.ID]
			if !ok {
				depErr.Deletes = append(depErr.Deletes, d)
				continue
			}
			d.Queue = q
		}
		if !addLock(d.Queue, false) {
			depErr.Deletes = append(depErr.Deletes, d)
		}
	}

	for _, d := range mod.Depends {
		if d.Queue == "" {
			q, ok := m.byID[d.ID]
			if !ok {
				depErr.Depends = append(depErr.Depends, d)
				continue
			}
		}
		if !addLock(d.Queue, false) {
			depErr.Depends = append(depErr.Depends, d)
		}
	}

	delNew := func() {
		if len(newQs) == 0 {
			return
		}
		// This is ticklish. The only reason we can acquire this lock later,
		// while the queue locks are held, is that it will *always be last in
		// line* after the queues. When it's acquired in Modify and Claim at
		// the beginning, it is *also released*. Thus, we avoid deadlock
		// conditions because the only time this lock is held simultaneously
		// with queue locks is in this unique case at the *end*, and all
		// modifications hold it in this way.
		defer un(lock(m))
		for _, q := range newQs {
			delete(m.locks[q])
		}
	}

	if depErr.HasMissing() {
		// Delete all new queue locks and return an error.
		for _, q := range newQs {
			delete(m.locks[q])
		}
		return nil, nil, depErr
	}

	// We have all of the locks we need. Sort to avoid dining philosophers problems.
	sort.Slice(locks, func(i, j int) bool {
		return locks[i].q < locks[j].q
	})

	return locks, delNew, nil
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
	//
	// - Modify claimHeaps and actual tasks. Take special care with deletions and insertions.
	// - Unlock queues.

	qls, delNew, err := m.modLocks(mod)
	if err != nil {
		return nil, nil, errors.Wrap(err, "eqmem modify")
	}

	// Now lock all of the queues and defer unlocking them at the same time.
	for _, ql := range qls {
		defer un(lock(ql))
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

	// Now that we have all of the relevnt queues locked, we can check modification validity.
	// Steps:
	// - Gather all dependencies that we can find.
	// - Create a dependency error and bail if it's non-empty.
	// - Perform modifications.
	// - - For things that have At or Claimant changed,
	// 	   the corresponding heap needs to be updated for the given ID.
	// - - For things that have their queue changed or are inserted or deleted,
	//     corresponding modifications must be made to all affected heaps.
	// - deferrals handle unlocking in the proper order.

	// TODO
	// TODO
	// TODO
	// TODO
	// TODO
	// TODO
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
func (m *EQMem) Queues(ctx context.Context, qq *entroq.Query) (map[string]int, error) {
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

// Tasks are actually in a two-layered structure: map[queue]map[id]task. If we do that with sync.Map structures (both of them), then readers can access it without locking the whole structure: both listings and stats can just iterate over them and we're all set!
//
// A simple ID->queue structure, which would be locked all at once, can be used for finding queues from only IDs: map[id]queue (with its own lock)
//
// Now we're talking about a main data structure and two indices, with possibly a 4th for holding queue-oriented locks:
//
// - map[queue]map[id]task <- sync.Map, no main locks
// - map[id]struct{version, queue, claimant, at} <- has its own "global" lock for lookups
// - map[queue]heap (or similar) <- has its own "global" lock
// - map[queue]lock? <- the "truly required" modification locks. All queues referenced in a claim or modify operation have to be held to do anything. A global lock on this structure would also be required to list queues, etc.
//
// If we view it in this way, now we have the following behaviors:
//
// Claim:
// - Lock "lock map"
// - Get queue locks and names that match from that map
// - Unlock "lock map"
// - In turn, lock for each queue, then access heap/btree to find task to claim
// - Update actual task in queue-task map
// - Update the heap/btree because of new arrival time
// - Release lock
//
// Note that claim operations never alter the queue of a task, and thus they never touch the id->queue map. They do change the arrival time, so they cause a heap event on that queue.
//
// Modify:
// - Lock "lock map"
// - Get queue locks and names that match modification from map
// - DO NOT unlock "lock map" <- modification can change it by insertion and deletion, and we don't want another modification to insert into a queue and create an entry (or delete one that's there), that will confuse us.
// - Lock all relevant queues in sorted order
// - Modify queue-task map, ID-heap-index-queue map, and queue-heap map
// - Possibly modify the lock map, adding or deleting a queue entry
// - Unlock "lock map"
//
// List:
// - Just iterate over the queue-task map. Done
//
// Stats:
// - Just iterate over queue-task maps.
//
