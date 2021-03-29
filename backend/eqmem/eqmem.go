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
	"github.com/pkg/errors"
)

type EQMem struct {
	sync.Mutex

	nw entroq.NotifyWaiter

	// queues allows tasks to be accessed by queue name. The returned type is
	// safe for concurrent use, and follows sync.Map semantics.
	queues map[string]*taskQueue

	// qByID gets the queue name for a given task ID. This is used to quickly
	// look up tasks when the queue name is unknown. That should never be the
	// case, however, since modifications are done on existing tasks, and have
	// to go through RBAC based on queue names.
	qByID map[uuid.UUID]string

	// locks contains lockers for each known queue. The locks know their own
	// queue name, as well.
	locks map[string]*qLock
}

type qLock struct {
	sync.Mutex
	queue string
	heap  *claimHeap
	tasks *taskQueue
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
		nw:     subq.New(),
		queues: make(map[string]*taskQueue),
		qByID:  make(map[uuid.UUID]string),
		locks:  make(map[string]*qLock),
	}
}

// claimPrep prepares for attempting to claim a task by very briefly locking
// the global data structures, just to get a handle on the exact queue locks
// and heaps that are needed.
func (m *EQMem) claimPrep(cq *entroq.ClaimQuery) []*qLock {
	defer un(lock(m))

	var qls []*qLock
	for _, q := range cq.Queues {
		if ql, ok := m.locks[q]; ok {
			qls = append(qls, ql)
		}
	}
	return qls
}

// mustTryClaimOne attempts to make a claim on exactly one queue using the
// provided indexing lock structure. If there is some kind of error it will be
// because of an inconsistent state (a bug), and therefore errors are fatal
// here.
func (m *EQMem) mustTryClaimOne(ql *qLock, now time.Time, cq *entroq.ClaimQuery) *entroq.Task {
	defer un(lock(ql))

	item := ql.heap.RandomAvailable(now)
	if item == nil {
		return nil
	}

	// Found one - time to modify it for claiming and return it.
	// We are under the queue lock for this task's queue, so we now have to
	// - Update the task at+claimant in the corresponding heap.
	// - Update the task itself in the task store.
	newAt := now.Add(cq.Duration)
	ql.heap.UpdateItem(item, newAt)

	qts, ok := m.queueTasks(ql.queue)
	if !ok {
		log.Fatalf("Inconsistent internal state: could not find queue %q after finding a claimable task in it", ql.queue)
	}

	var found *entroq.Task
	if err := qts.Update(item.id, func(t *entroq.Task) *entroq.Task {
		t = t.Copy() // avoid data race, don't change in place
		t.At = newAt
		t.Claimant = cq.Claimant
		t.Version++
		t.Claims++
		t.Modified = now

		found = t
		return t
	}); err != nil {
		log.Fatalf("Inconsistent internal state: could not update task in %q after claim started", ql.queue)
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

	qls := m.claimPrep(cq)
	if len(qls) == 0 {
		return nil, nil
	}

	now, err := m.Time(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "eqmem claim time")
	}

	// Shuffle to avoid favoring one queue.
	rand.Shuffle(len(qls), func(i, j int) {
		qls[i], qls[j] = qls[j], qls[i]
	})

	for _, ql := range qls {
		if task := m.mustTryClaimOne(ql, now, cq); task != nil {
			return task, nil
		}
	}

	return nil, nil
}

func (m *EQMem) unsafeEnsureQueue(q string) {
	ts, ok := m.queues[q]
	if ts == nil || !ok {
		ts = newTaskQueue(q)
		m.queues[q] = ts
	}
	if ql, ok := m.locks[q]; ql == nil || !ok {
		m.locks[q] = &qLock{
			queue: q,
			heap:  newClaimHeap(),
			tasks: ts,
		}
	}
}

func (m *EQMem) unsafeCleanQueue(q string) {
	ts, ok := m.queues[q]
	if ok && ts.Len() == 0 {
		delete(m.queues, q)
		delete(m.locks, q)
	}
}

func ensureModQueues(mod *entroq.Modification, qByID map[uuid.UUID]string) {
	for _, d := range mod.Deletes {
		if d.Queue == "" {
			d.Queue = qByID[d.ID]
		}
	}

	for _, d := range mod.Depends {
		if d.Queue == "" {
			d.Queue = qByID[d.ID]
		}
	}

	for _, c := range mod.Changes {
		if c.FromQueue == "" {
			c.FromQueue = qByID[c.ID]
		}
	}
}

// modPrep finds all queues from a particular modification request. If any of
// the given queues are not found, then it returns a "not okay" value as the
// second parameter. Otherwise it returns a list of queue locks that can be
// locked in the caller when ready. It can create new locks (e.g., for
// insertions). The modification is altered in this call to ensure that
// everything for which a queue can be found has one (e.g., deletions that have
// only IDs will get a queue here if they can be found).
//
// Also, if any queue indexes don't have a queue represented, that is fixed here.
func (m *EQMem) modPrep(mod *entroq.Modification) []*qLock {
	// This has to be locked the whole time so that IDs and queues are matched
	// properly if queues are missing somewhere.
	defer un(lock(m))

	ensureModQueues(mod, m.qByID)
	queues := make(map[string]bool)
	for _, ins := range mod.Inserts {
		queues[ins.Queue] = true
	}
	for _, c := range mod.Changes {
		queues[c.FromQueue] = true
		queues[c.Queue] = true
	}
	for _, d := range mod.Deletes {
		queues[d.Queue] = true
	}
	for _, d := range mod.Depends {
		queues[d.Queue] = true
	}

	var locks []*qLock
	for q := range queues {
		if q == "" {
			continue
		}
		m.unsafeEnsureQueue(q)
		locks = append(locks, m.locks[q])
	}

	// We have all of the locks we need. Sort to avoid dining philosophers problems.
	sort.Slice(locks, func(i, j int) bool {
		return locks[i].queue < locks[j].queue
	})

	return locks
}

// Modify attempts to do an atomic modification on the system, given a
// particular set of modification information (deletions, changes, insertions,
// dependencies).
func (m *EQMem) Modify(ctx context.Context, mod *entroq.Modification) (inserted, changed []*entroq.Task, err error) {
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
	queueLocks := m.modPrep(mod)
	if len(queueLocks) == 0 {
		return nil, nil, nil
	}

	// Lock all queues, and store them in a place that's easy to look up.
	byQ := make(map[string]*qLock)
	for _, ql := range queueLocks {
		defer un(lock(ql))
		byQ[ql.queue] = ql
	}

	// Set things up to delete empty queues if any were left empty after the
	// modification. We don't do this as we go along because it is quite
	// possible to have one task deleted from a queue, making it empty, and
	// another added in the same transaction.
	defer func() {
		defer un(lock(m))
		for _, ql := range queueLocks {
			m.unsafeCleanQueue(ql.queue)
		}
	}()

	// Find the actual tasks involved. Queues were filled in when obtaining locks.
	found := make(map[uuid.UUID]*entroq.Task)
	addFound := func(q string, id uuid.UUID) {
		if q == "" || id == uuid.Nil {
			return
		}
		if ql, ok := byQ[q]; ok {
			if t, ok := ql.tasks.Get(id); ok {
				found[t.ID] = t
			}
		}
	}
	for _, d := range mod.Deletes {
		addFound(d.Queue, d.ID)
	}
	for _, d := range mod.Depends {
		addFound(d.Queue, d.ID)
	}
	for _, c := range mod.Changes {
		addFound(c.FromQueue, c.ID)
	}
	for _, t := range mod.Inserts {
		addFound(t.Queue, t.ID)
	}

	if err := mod.DependencyError(found); err != nil {
		return nil, nil, errors.Wrap(err, "eqmem modify")
	}

	// Now that we know we can proceed with our process, make all of the necessary changes.
	// We got all of the queue-based stuff handed to us previously, so we
	// already hold all of the locks for that stuff and can edit with impunity.

	var finalLockedSteps []func()

	deleteID := func(q string, id uuid.UUID) {
		ql := byQ[q]
		ql.heap.RemoveID(id)
		ql.tasks.Delete(id)

		finalLockedSteps = append(finalLockedSteps, func() {
			delete(m.qByID, id)
		})
	}

	insertTask := func(t *entroq.Task) {
		ql := byQ[t.Queue]
		ql.heap.PushItem(newItem(ql.queue, t.ID, t.At))
		ql.tasks.Set(t.ID, t)

		finalLockedSteps = append(finalLockedSteps, func() {
			m.qByID[t.ID] = t.Queue
		})
	}

	updateTask := func(t *entroq.Task) {
		ql := byQ[t.Queue]
		if ok := ql.heap.UpdateID(t.ID, t.At); !ok {
			log.Fatalf("Inconsistent state: task %v not found in queue heap %q", t.ID, t.Queue)
		}
		ql.tasks.Set(t.ID, t)
	}

	now, err := m.Time(ctx)
	if err != nil {
		return nil, nil, errors.Wrap(err, "modify get time")
	}

	for _, d := range mod.Deletes {
		deleteID(d.Queue, d.ID)
	}
	for _, c := range mod.Changes {
		newTask := c.Copy()
		newTask.Version++
		newTask.Claimant = mod.Claimant
		newTask.Modified = now
		if c.FromQueue != c.Queue {
			deleteID(c.FromQueue, c.ID)
			insertTask(newTask)
		} else {
			// Original version was already checked earlier.
			updateTask(newTask)
		}
		changed = append(changed, newTask)
	}
	for _, td := range mod.Inserts {
		id := td.ID
		if id == uuid.Nil {
			id = uuid.New()
		}
		newTask := &entroq.Task{
			ID:       id,
			Queue:    td.Queue,
			At:       td.At,
			Value:    td.Value,
			Claimant: mod.Claimant,
			Created:  now,
			Modified: now,
		}
		insertTask(newTask)
		inserted = append(inserted, newTask)
	}

	func() {
		defer un(lock(m))
		for _, step := range finalLockedSteps {
			step()
		}
	}()

	entroq.NotifyModified(m.nw, inserted, changed)

	// All done!
	return inserted, changed, nil
}

// Time returns the current time.
func (m *EQMem) Time(_ context.Context) (time.Time, error) {
	return entroq.ProcessTime(), nil
}

func (m *EQMem) queueForID(id uuid.UUID) (string, bool) {
	defer un(lock(m))
	q, ok := m.qByID[id]
	return q, ok
}

func (m *EQMem) queueTasks(queue string) (*taskQueue, bool) {
	defer un(lock(m))
	q, ok := m.queues[queue]
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
		found = append(found, t.CopyWithValue(!tq.OmitValues))
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
			tryAdd(t)
		}
		return found, nil
	}

	// No ID list, just a queue, range over it.
	qts, ok := m.queueTasks(tq.Queue)
	if !ok {
		return nil, nil
	}

	qts.Range(func(_ uuid.UUID, t *entroq.Task) bool {
		if tq.Limit != 0 && tq.Limit <= len(found) {
			return false
		}
		tryAdd(t)
		return true
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
	now, err := m.Time(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "queue stats time")
	}
	var qnames []string
	func() {
		defer un(lock(m))
		for q := range m.queues {
			qnames = append(qnames, q)
		}
	}()

	qs := make(map[string]*entroq.QueueStat)
	for _, q := range qnames {
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
		qts.Range(func(_ uuid.UUID, t *entroq.Task) bool {
			stats.Size++
			if t.At.After(now) {
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
