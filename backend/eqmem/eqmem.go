// Package eqmem implements an in-memory entroq that has fine-grained locking
// and can handle simultaneously stats/task listing and modifications to a
// large extent.
package eqmem

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/subq"
	"github.com/shiblon/stuffedio/wal"
)

type EQMem struct {
	sync.Mutex

	nw entroq.NotifyWaiter

	// queues allows tasks to be accessed by queue name. The returned type is
	// safe for concurrent use, and follows sync.Map semantics.
	queues map[string]*taskQueue

	// qByID gets the queue name for a given task ID. This is used to quickly
	// look up tasks when the queue name is unknown. That should never be the
	// case, since modifications are done on existing tasks, and have
	// to go through RBAC based on queue names, but it is possible to try to
	// reinsert a task when it has been moved to another queue.
	qByID map[uuid.UUID]string

	// locksSuperUnsafe contains lockers for each known queue. The locks know
	// their own queue name, as well. Do not use directly. They require use of the
	// mutex to access *every time*.
	locksSuperUnsafe map[string]*qLock

	// A journaler, if one has been requested via a journal directory.
	journal *wal.WAL

	// journalDir, if non-empty, is expected to be a directory containing
	// journals and possibly snapshots for persisting EntroQ state.
	// Other options for journals are below.
	journalDir      string
	maxJournalBytes int64
	maxJournalItems int

	// outputSnapshot, if true, indicates tha the system should start up, read
	// journals, and dump a snapshot before closing itself down.
	outputSnapshot bool
}

type qLock struct {
	sync.Mutex
	queue string
	heap  *claimHeap
	tasks *taskQueue
	// Because we become dependent on the *existence* of the lock before we can
	// actually take it, and the global lock must be released before taking
	// this one, this gets incremented while we hold the global lock,
	// and decremented when unlocking the queue lock.
	dependents int
}

func lock(l sync.Locker) func() {
	l.Lock()
	return l.Unlock
}

func un(f func()) {
	f()
}

// Opener returns a constructor of the in-memory backend.
func Opener(opts ...Option) entroq.BackendOpener {
	return func(ctx context.Context) (entroq.Backend, error) {
		back, err := New(ctx, opts...)
		return back, err
	}
}

// Option represents options for creationg of the in-memory implementation.
type Option func(*EQMem)

// WithJournal sets up a file-based journal system so that the in-memory
// implementation can be persisted.
func WithJournal(dir string) Option {
	return func(m *EQMem) {
		m.journalDir = dir
	}
}

// WithMaxJournalBytes sets a maximum on the number of bytes before rotation.
// Default is wal.DefaultMaxBytes.
func WithMaxJournalBytes(max int64) Option {
	return func(m *EQMem) {
		if max <= 0 {
			return
		}
		m.maxJournalBytes = max
	}
}

// WithMaxJournalItems sets a maximum on the number of entries in the journal
// before rotation. Default is wal.DefaultMaxIndices.
func WithMaxJournalItems(max int) Option {
	return func(m *EQMem) {
		if max <= 0 {
			return
		}
		m.maxJournalItems = max
	}
}

// withOutputSnapshot causes the journal to be loaded (without live files), a
// snapshot to be created, and then the system to be closed.
// This is private to avoid mistakes and misuse, since setting this places the
// system into a state where it cannot safely be used afterward.
// Use TakeSnapshot to get this behavior.
func withOutputSnapshot() Option {
	return func(m *EQMem) {
		m.outputSnapshot = true
	}
}

// New returns a new in-memory implementation, ready to be used.
func New(ctx context.Context, opts ...Option) (*EQMem, error) {
	m := &EQMem{
		nw:               subq.New(),
		queues:           make(map[string]*taskQueue),
		qByID:            make(map[uuid.UUID]string),
		locksSuperUnsafe: make(map[string]*qLock),
	}
	for _, opt := range opts {
		opt(m)
	}

	// If we have a journal dir, then we can use it.
	if m.journalDir != "" {
		walOpts := []wal.Option{
			wal.WithMaxJournalBytes(m.maxJournalBytes),
			wal.WithMaxJournalIndices(m.maxJournalItems),
			wal.WithAllowWrite(!m.outputSnapshot),
			wal.WithExcludeLiveJournal(m.outputSnapshot),
			wal.WithSnapshotLoaderFunc(func(ctx context.Context, b []byte) error {
				task := new(entroq.Task)
				if err := json.Unmarshal(b, task); err != nil {
					return fmt.Errorf("eqmem load task: %w", err)
				}

				qls, unlock := m.lockQueues([]string{task.Queue})
				defer unlock()

				m.queueUnsafeInsertTask(qls[0], task)

				return nil
			}),
			wal.WithJournalPlayerFunc(func(ctx context.Context, b []byte) error {
				mod := new(entroq.Modification)
				if err := json.Unmarshal(b, mod); err != nil {
					return fmt.Errorf("eqmem play mod: %w", err)
				}

				// Since changes represent the *final state* in the journal, we
				// decrement the version number before attempting to apply the
				// modification.
				for _, chg := range mod.Changes {
					chg.Version--
				}

				if _, _, err := m.modifyImpl(ctx, mod, true); err != nil {
					return fmt.Errorf("eqmem play mod: %w", err)
				}
				return nil
			}),
		}
		var err error
		if m.journal, err = wal.Open(ctx, m.journalDir, walOpts...); err != nil {
			return nil, fmt.Errorf("open WAL: %w", err)
		}

		// Now it's loaded. If we are to output a snapshot, then we create it
		// here and close the whole system down.
		if m.outputSnapshot {
			if !m.journal.SnapshotUseful() {
				log.Printf("Snapshot requested, but not useful: empty, or frozen journals already collapsed")
				return m, nil
			}
			if _, err := m.journal.CreateSnapshot(m.makeSnapshot); err != nil {
				return nil, fmt.Errorf("output snapshot: %w", err)
			}
		}
	}

	return m, nil
}

// TakeSnapshot brings the system up empty, loads a snapshot + journals,
// then outputs a new snapshot and exits. Cleans up old files after
// snapshotting if requested. Otherwise they are just moved out of the way.
func TakeSnapshot(ctx context.Context, journalDir string, cleanup bool) error {
	m, err := New(ctx, WithJournal(journalDir), withOutputSnapshot())
	if err != nil {
		return fmt.Errorf("load for snapshot: %w", err)
	}
	defer m.Close()
	if cleanup {
		if err := wal.Cleanup(journalDir); err != nil {
			return fmt.Errorf("snapshot cleanup: %w", err)
		}
	}
	return nil
}

func (m *EQMem) makeSnapshot(a wal.ValueAdder) error {
	var err error
	for _, ts := range m.queues {
		ts.Range(func(_ uuid.UUID, t *entroq.Task) bool {
			var b []byte
			if b, err = json.Marshal(t); err != nil {
				err = fmt.Errorf("marshal for snapshot: %w", err)
				return false
			}
			if err = a.AddValue(b); err != nil {
				err = fmt.Errorf("add value: %w", err)
				return false
			}
			return true
		})
	}
	return err
}

func (m *EQMem) queueLen(q string) int {
	defer un(lock(m))
	return m.queues[q].Len()
}

// mustTryClaimOne attempts to make a claim on exactly one queue using the
// provided indexing lock structure. If there is some kind of error it will be
// because of an inconsistent state (a bug), and therefore errors are fatal
// here.
func (m *EQMem) mustTryClaimOne(q string, now time.Time, cq *entroq.ClaimQuery) *entroq.Task {
	if m.queueLen(q) == 0 {
		return nil
	}
	qls, unlock := m.lockQueues([]string{q})
	defer unlock()

	ql := qls[0]

	item := ql.heap.RandomAvailable(now)
	if item == nil {
		return nil
	}

	qts, ok := m.queueTasks(ql.queue)
	if !ok {
		log.Fatalf("Inconsistent internal state: could not find queue %q after finding a claimable task in it", ql.queue)
	}

	// Found one - time to modify it for claiming and return it.
	// We are under the queue lock for this task's queue, so we now have to
	// - Update the task at+claimant in the corresponding heap.
	// - Update the task itself in the task store.
	newAt := now.Add(cq.Duration)
	ql.heap.UpdateItem(item, newAt)

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

	if m.journal != nil {
		// Update for claim. Note that we need the final state, not the
		// original version. Journal playback decrements the version number by
		// 1 when applying modifications.
		mod := &entroq.Modification{
			Claimant: cq.Claimant,
			Changes:  []*entroq.Task{found},
		}
		// Marshal mod and store in journal.
		b, err := json.Marshal(mod)
		if err != nil {
			log.Fatalf("Inconsistent internal state: updated task but couldn't marshal JSON: %v", err)
		}
		if err := m.journal.Append(b); err != nil {
			log.Fatalf("Inconsistent internal state: updated task but couldn't write to journal: %v", err)
		}
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

	queues := make([]string, len(cq.Queues))
	copy(queues, cq.Queues)

	// Shuffle to avoid favoring one queue.
	rand.Shuffle(len(queues), func(i, j int) {
		queues[i], queues[j] = queues[j], queues[i]
	})

	now, err := m.Time(ctx)
	if err != nil {
		return nil, fmt.Errorf("eqmem claim time: %w", err)
	}

	for _, q := range queues {
		if task := m.mustTryClaimOne(q, now, cq); task != nil {
			return task, nil
		}
	}

	return nil, nil
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
		// Always from where it already is. Always.
		c.FromQueue = qByID[c.ID]
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
func (m *EQMem) modPrep(mod *entroq.Modification) (sortedQueues []string, misplacedInsIDs map[uuid.UUID]string) {
	// This has to be locked the whole time so that IDs and queues are matched
	// properly if queues are missing somewhere.
	defer un(lock(m))

	misplacedInsIDs = make(map[uuid.UUID]string)

	ensureModQueues(mod, m.qByID)
	queues := make(map[string]bool)
	for _, ins := range mod.Inserts {
		// If we have an ID to insert, find the queue for that task to return it.
		// Also make sure we get the lock for that queue.
		if ins.ID != uuid.Nil {
			if foundQueue, ok := m.qByID[ins.ID]; ok && foundQueue != ins.Queue {
				misplacedInsIDs[ins.ID] = foundQueue
				queues[foundQueue] = true
			}
		}
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

	delete(queues, "") // in case there's an empty queue in there.

	// We have all of the locks we need. Sort to avoid dining philosophers problems.
	for q := range queues {
		sortedQueues = append(sortedQueues, q)
	}
	sort.Strings(sortedQueues)

	return sortedQueues, misplacedInsIDs
}

// queueUnsafeInsertTask performs queue-level operations on a task, then
// returns a function to call under global lock to finish the job.
func (m *EQMem) queueUnsafeInsertTask(ql *qLock, t *entroq.Task) func() {
	ql.heap.PushItem(newItem(ql.queue, t.ID, t.At))
	ql.tasks.Set(t.ID, t)
	return func() {
		m.qByID[t.ID] = t.Queue
	}
}

// queueUnsafeDeleteID performs a queue-level deletion operation, then returns
// a function to be called under the global lock to finish the job.
func (m *EQMem) queueUnsafeDeleteID(ql *qLock, id uuid.UUID) func() {
	ql.heap.RemoveID(id)
	ql.tasks.Delete(id)
	return func() {
		delete(m.qByID, id)
	}
}

// queueUnsafeUpdateTask performs a queue-level task update. Note that if the
// queue changes, insert and delete should be used instead. This is same-queue
// only. Returns a function to be called to finish global fixups, if needed.
func (m *EQMem) queueUnsafeUpdateTask(ql *qLock, t *entroq.Task) func() {
	if ok := ql.heap.UpdateID(t.ID, t.At); !ok {
		log.Fatalf("Inconsistent state: task %v not found in queue heap %q for update", t.ID, t.Queue)
	}
	ql.tasks.Set(t.ID, t)
	// Nothing to do at present.
	return nil
}

// Modify attempts to do an atomic modification on the system, given a
// particular set of modification information (deletions, changes, insertions,
// dependencies).
func (m *EQMem) Modify(ctx context.Context, mod *entroq.Modification) (inserted, changed []*entroq.Task, err error) {
	return m.modifyImpl(ctx, mod, false)
}

func (m *EQMem) modifyImpl(ctx context.Context, mod *entroq.Modification, ignoreClaimant bool) (inserted, changed []*entroq.Task, err error) {
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
	// - Lock all queue locks, hold them for the rest of the function (unlock at the end).
	// - Sometimes it's okay to grab the global lock before manipulating global
	//   indices (it will have already been released, and it always comes after
	//   the queue locks have been obtained).
	//
	// - Modify claimHeaps and actual tasks. Take special care with deletions and insertions.
	// - Unlock queues.
	queues, misplacedInsIDs := m.modPrep(mod)
	if len(queues) == 0 {
		return nil, nil, nil
	}

	// Lock all queues, and store them in a place that's easy to look up.
	byQ := make(map[string]*qLock)
	qls, unlockQueues := m.lockQueues(queues)
	defer unlockQueues()
	for _, ql := range qls {
		byQ[ql.queue] = ql
	}

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
		// If the insert task ID was found in *another* queue, we still want to
		// know that. This can happen if a task is moved, and then an attempt
		// is made to reinsert it. This should cause a collision.
		if q, ok := misplacedInsIDs[t.ID]; ok {
			addFound(q, t.ID)
		}
	}

	if err := mod.DependencyError(found); err != nil {
		depErr, ok := entroq.AsDependency(err)
		// If we are doing special "ignore claimant" work (like reading from a
		// journal), then only throw an error if we have something other than
		// claim problems or something other than a dependency error.
		if !ignoreClaimant || !ok || !depErr.OnlyClaims() {
			return nil, nil, fmt.Errorf("eqmem modify: %w", err)
		}
	}

	// Now that we know we can proceed with our process, make all of the necessary changes.
	// We got all of the queue-based stuff handed to us previously, so we
	// already hold all of the locks for that stuff and can edit with impunity.

	var finalLockedSteps []func()

	deleteID := func(q string, id uuid.UUID) {
		ql := byQ[q]
		finalLockedSteps = append(finalLockedSteps, m.queueUnsafeDeleteID(ql, id))
	}

	insertTask := func(t *entroq.Task) {
		ql := byQ[t.Queue]
		finalLockedSteps = append(finalLockedSteps, m.queueUnsafeInsertTask(ql, t))
	}

	updateTask := func(t *entroq.Task) {
		ql := byQ[t.Queue]
		finalLockedSteps = append(finalLockedSteps, m.queueUnsafeUpdateTask(ql, t))
	}

	now, err := m.Time(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("modify get time: %w", err)
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
		// Restore timings if we're reading from a journal.
		created := td.Created
		if created.IsZero() {
			created = now
		}
		modified := td.Modified
		if modified.IsZero() {
			modified = now
		}
		newTask := &entroq.Task{
			ID:       id,
			Queue:    td.Queue,
			At:       td.At,
			Value:    td.Value,
			Claimant: mod.Claimant,
			Created:  created,
			Modified: modified,
		}
		insertTask(newTask)
		inserted = append(inserted, newTask)
	}

	func() {
		defer un(lock(m))
		for _, step := range finalLockedSteps {
			if step != nil {
				step()
			}
		}
	}()

	// Now we can journal things. We have to take care to use what was actually
	// inserted and what was actually changed.
	// Note that this means that what is journaled has a version *1 ahead* of
	// the version it is looking for. Thus, when reading journal entries, we
	// decrement the version by one before applying a modification. What's
	// journaled is the final state.
	if m.journal != nil {
		jMod := &entroq.Modification{
			Claimant: mod.Claimant,
			Deletes:  mod.Deletes,
			Depends:  mod.Depends,
			Changes:  changed,
		}
		// TODO: this messes with the timestamps! It would be better if we
		// could restore created/modified.
		for _, ins := range inserted {
			jMod.Inserts = append(jMod.Inserts, ins.Data())
		}
		b, err := json.Marshal(jMod)
		if err != nil {
			log.Fatalf("Inconsistent internal state: modification succeeded but could not marshal JSON: %v", err)
		}
		if err := m.journal.Append(b); err != nil {
			log.Fatalf("Inconsistent internal state: modification succeeded but could not append to journal: %v", err)
		}
	}

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
		return nil, fmt.Errorf("eqmem tasks time: %w", err)
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
		return nil, fmt.Errorf("queue stats time: %w", err)
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
func (m *EQMem) Close() error {
	if m.journal != nil {
		err := m.journal.Close()
		m.journal = nil
		return err
	}
	return nil
}

// Obtain lock structures for every queue, creating them as needed and
// incrementing dependents.
func (m *EQMem) locksForQueues(qs []string) []*qLock {
	defer un(lock(m))
	var locks []*qLock
	for _, q := range qs {
		locks = append(locks, m.lockForQueueUnsafe(q))
	}
	return locks
}

// Get a single lock for a queue, creating it if it doesn't exist. Dependents
// are incremented here.
func (m *EQMem) lockForQueueUnsafe(q string) (ql *qLock) {
	// Always increment dependents, whether we exit early from finding a lock,
	// or late from creating a new queue.
	defer func() {
		ql.dependents++
	}()

	ql = m.locksSuperUnsafe[q]

	if ts := m.queues[q]; (ts == nil) != (ql == nil) {
		log.Fatalf("Queue tasks and lock structures out of step for queue %q: ts=%v, ql=%v", q, ts, ql)
	}

	if ql != nil {
		return ql
	}

	ts := newTaskQueue(q)
	m.queues[q] = ts

	ql = &qLock{
		queue: q,
		heap:  newClaimHeap(),
		tasks: ts,
	}
	m.locksSuperUnsafe[q] = ql

	return ql
}

// lockQueues locks a particular slice of queues in order and returns the lock
// structures. If the lock doesn't exist, it creates it. It holds the global
// mutex during the collection operation, markes the queues as depended on,
// releases the lock, and finally locks the queue locks themselves.
// It returns a list of locks and a function to unlock them in the proper way,
// decrementing dependents and avoiding multi-lock race conditions.
func (m *EQMem) lockQueues(qs []string) ([]*qLock, func()) {
	if len(qs) == 0 {
		return nil, func() {}
	}
	qls := m.locksForQueues(qs)
	for _, ql := range qls {
		ql.Lock()
	}

	return qls, func() {
		// Unlock in reverse order.
		for i := len(qls) - 1; i >= 0; i-- {
			qls[i].Unlock()
		}
		// Now that we're unlocked, take the global lock again and reduce
		// dependents by 1, in reverse order, then try to clean up if
		// dependents go to zero anywhere with empty queues. If it fails, it
		// simply exits; something else needed the lock to stay alive betwen
		// lock acquisitions, so cleanup will occur later.
		defer un(lock(m))
		for i := len(qls) - 1; i >= 0; i-- {
			ql := qls[i]
			ql.dependents--

			if ts := m.queues[ql.queue]; ql.dependents == 0 && ts.Len() == 0 {
				delete(m.queues, ql.queue)
				delete(m.locksSuperUnsafe, ql.queue)
			}
		}
	}
}
