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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/gcmetrics"
	"github.com/shiblon/entroq/pkg/subq"
	"github.com/shiblon/stuffedio/wal"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

type EQMem struct {
	sync.Mutex

	nw entroq.NotifyWaiter

	// queues allows tasks to be accessed by queue name. The returned type is
	// safe for concurrent use, and follows sync.Map semantics.
	queues map[string]*taskQueue

	// namespaces allows resources to be accessed by namespace name.
	namespaces map[string]*docNamespace

	// qByID gets the queue name for a given task ID. This is used to quickly
	// look up tasks when the queue name is unknown. That should never be the
	// case, since modifications are done on existing tasks, and have
	// to go through RBAC based on queue names, but it is possible to try to
	// reinsert a task when it has been moved to another queue.
	qByID map[string]string

	// locksSuperUnsafe contains lockers for each known queue. The locks know
	// their own queue name, as well. Do not use directly. They require use of the
	// mutex to access *every time*.
	locksSuperUnsafe map[string]*qLock

	// locksSuperUnsafeNS contains lockers for each known namespace.
	locksSuperUnsafeNS map[string]*nsLock

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

	claimDuration  metric.Float64Histogram
	modifyDuration metric.Float64Histogram
	gcMetrics      *gcmetrics.Metrics

	gcInterval  time.Duration
	gcBatchSize int
	stopGC      func()
	gcDone      chan struct{}
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

type nsLock struct {
	sync.Mutex
	namespace string
	docs      *docNamespace
	// Because we become dependent on the *existence* of the lock before we can
	// actually take it, and the global lock must be released before taking
	// this one, this gets incremented while we hold the global lock,
	// and decremented when unlocking the namespace lock.
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

// WithMeterProvider sets the OTel MeterProvider for claim and modify duration
// histograms. Defaults to a noop provider.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(m *EQMem) {
		m.claimDuration, _ = mp.Meter("entroq.mem").Float64Histogram("entroq.claim.duration",
			metric.WithDescription("Duration of TryClaim calls in the in-memory backend."),
			metric.WithUnit("s"),
		)
		m.modifyDuration, _ = mp.Meter("entroq.mem").Float64Histogram("entroq.modify.duration",
			metric.WithDescription("Duration of Modify calls in the in-memory backend."),
			metric.WithUnit("s"),
		)
		m.gcMetrics, _ = gcmetrics.New(mp.Meter("entroq.mem"))
	}
}

// New returns a new in-memory implementation, ready to be used.
func New(ctx context.Context, opts ...Option) (*EQMem, error) {
	noopMeter := noop.NewMeterProvider().Meter("entroq.mem")
	claimDuration, _ := noopMeter.Float64Histogram("entroq.claim.duration")
	modifyDuration, _ := noopMeter.Float64Histogram("entroq.modify.duration")
	gcMetrics, _ := gcmetrics.New(noopMeter)

	m := &EQMem{
		nw:                 subq.New(),
		queues:             make(map[string]*taskQueue),
		namespaces:         make(map[string]*docNamespace),
		qByID:              make(map[string]string),
		locksSuperUnsafe:   make(map[string]*qLock),
		locksSuperUnsafeNS: make(map[string]*nsLock),
		claimDuration:      claimDuration,
		modifyDuration:     modifyDuration,
		gcMetrics:          gcMetrics,
		gcInterval:         defaultGCInterval,
		gcBatchSize:        defaultGCBatchSize,
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
				// modification so the version-check in DependencyError passes.
				for _, chg := range mod.Changes {
					chg.Version--
				}
				for _, chg := range mod.DocChanges {
					chg.Version--
				}

				if _, err := m.modifyImpl(ctx, mod, true); err != nil {
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

	// Garbage collection is a first-class, always-on backend behavior. It is not
	// started in snapshot-and-quit mode (a load-dump-exit tool). Its context is
	// rooted at context.Background(), NOT the constructor's ctx: the loop's
	// lifetime is the backend's, ended by Close, whereas the constructor ctx
	// scopes only construction (a caller may bound New with a timeout and defer
	// cancel()). Close cancels it and waits for it to exit.
	if !m.outputSnapshot {
		gcCtx, cancel := context.WithCancel(context.Background())
		m.stopGC = cancel
		m.gcDone = make(chan struct{})
		go func() {
			defer close(m.gcDone)
			m.runGCLoop(gcCtx, m.gcInterval, m.gcBatchSize)
		}()
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
		ts.Range(func(_ string, t *entroq.Task) bool {
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

	// Found one - time to modify it for claiming and return it.
	// We are under the queue lock for this task's queue, so we now have to
	// - Update the task at+claimant in the corresponding heap.
	// - Update the task itself in the task store.
	newAt := now.Add(cq.Duration)
	ql.heap.UpdateItem(item, newAt)

	var found *entroq.Task
	if err := ql.tasks.Update(item.id, func(t *entroq.Task) *entroq.Task {
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
		//
		// The journaled change must name the task's current queue as its source
		// (FromQueue) so replay passes the queue-as-modify-key check: a claim
		// does not move the task, so source == destination. Journal a copy so the
		// task returned to the caller is not given a spurious FromQueue.
		jChange := found.Copy()
		jChange.FromQueue = jChange.Queue
		mod := &entroq.Modification{
			Claimant: cq.Claimant,
			Changes:  []*entroq.Task{jChange},
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
	start := time.Now()
	defer func() {
		m.claimDuration.Record(ctx, time.Since(start).Seconds())
	}()
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

// ensureModQueues enforces that every task operation names the queue the task
// actually lives in. Queues are EntroQ's authorization boundary, and authz runs
// against the queue the caller *names*, so if the operation could then act on a
// task in some other queue the ACL would be bypassable. We reject a mismatch
// (or an empty queue) rather than "helpfully" filling it in, since filling would
// defeat the check. A mismatch is reported as a DependencyError so an idempotent
// caller (e.g. re-deleting an already-gone task) sees the same "missing" signal
// it already handles, and a liar and a gone task look identical (no leak).
func ensureModQueues(mod *entroq.Modification, qByID map[string]string) error {
	depErr := new(entroq.DependencyError)
	for _, d := range mod.Deletes {
		if d.Queue == "" || d.Queue != qByID[d.ID] {
			depErr.Deletes = append(depErr.Deletes, entroq.NewTaskID(d.ID, d.Version, d.Queue))
		}
	}

	for _, d := range mod.Depends {
		if d.Queue == "" || d.Queue != qByID[d.ID] {
			depErr.Depends = append(depErr.Depends, entroq.NewTaskID(d.ID, d.Version, d.Queue))
		}
	}

	for _, c := range mod.Changes {
		if c.FromQueue == "" || c.FromQueue != qByID[c.ID] {
			depErr.Changes = append(depErr.Changes, entroq.NewTaskID(c.ID, c.Version, c.FromQueue))
		}
	}
	if len(depErr.Deletes)+len(depErr.Depends)+len(depErr.Changes) != 0 {
		depErr.Message = "modification queue does not match the task's current queue"
		return depErr
	}
	return nil
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
func (m *EQMem) modPrep(mod *entroq.Modification, replay bool) (queueNames, namespaceNames []string, misplacedInsIDs map[string]string, err error) {
	// This has to be locked the whole time so that IDs and queues are matched
	// properly if queues are missing somewhere.
	defer un(lock(m))

	misplacedInsIDs = make(map[string]string)

	// Journal replay re-applies the backend's own committed record, not an
	// external authorized request, so it is trusted. Journals written before the
	// queue-as-modify-key requirement (and claim changes in general) may omit an
	// op's queue; backfill it from stored state so the op both passes the
	// integrity check below and locates the right task when applied. Backfilling
	// is exactly what we forbid on the live write path (there it would defeat
	// authorization), which is why it is gated on replay only.
	if replay {
		for _, c := range mod.Changes {
			if c.FromQueue == "" {
				c.FromQueue = m.qByID[c.ID]
			}
		}
		for _, d := range mod.Deletes {
			if d.Queue == "" {
				d.Queue = m.qByID[d.ID]
			}
		}
		for _, d := range mod.Depends {
			if d.Queue == "" {
				d.Queue = m.qByID[d.ID]
			}
		}
	}

	if err := ensureModQueues(mod, m.qByID); err != nil {
		return nil, nil, nil, fmt.Errorf("mod prep: %w", err)
	}
	queues := make(map[string]bool)
	for _, ins := range mod.Inserts {
		// If we have an ID to insert, find the queue for that task to return it.
		// Also make sure we get the lock for that queue.
		if ins.ID != "" {
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

	// Collect the queue names to lock. lockQueues sorts them into a consistent
	// order to avoid dining-philosophers deadlock, so we do not sort here.
	for q := range queues {
		queueNames = append(queueNames, q)
	}

	// Collect the namespaces involved in doc operations, mirroring the queue
	// collection above; lockNamespaces sorts them for consistent lock ordering.
	namespaces := make(map[string]bool)
	for _, ins := range mod.DocInserts {
		namespaces[ins.Namespace] = true
	}
	for _, c := range mod.DocChanges {
		namespaces[c.Namespace] = true
	}
	for _, d := range mod.DocDeletes {
		namespaces[d.Namespace] = true
	}
	for _, d := range mod.DocDepends {
		namespaces[d.Namespace] = true
	}

	delete(namespaces, "")

	for n := range namespaces {
		namespaceNames = append(namespaceNames, n)
	}

	return queueNames, namespaceNames, misplacedInsIDs, nil
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
func (m *EQMem) queueUnsafeDeleteID(ql *qLock, id string) func() {
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

func (m *EQMem) Modify(ctx context.Context, mod *entroq.Modification) (*entroq.ModifyResponse, error) {
	start := time.Now()
	defer func() {
		m.modifyDuration.Record(ctx, time.Since(start).Seconds())
	}()
	return m.modifyImpl(ctx, mod, false)
}

// replay is true only when the journal player re-applies our own committed
// record (the sole such caller). Because that record is trusted rather than an
// external authorized request, replay both skips the claimant check and lets
// modPrep backfill queues that older journals omitted.
func (m *EQMem) modifyImpl(ctx context.Context, mod *entroq.Modification, replay bool) (*entroq.ModifyResponse, error) {
	// Double check that IDs are assigned.
	for _, t := range mod.Inserts {
		if t.ID == "" {
			return nil, fmt.Errorf("eqmem modify: task to insert is missing ID")
		}
	}
	for _, r := range mod.DocInserts {
		if r.ID == "" {
			return nil, fmt.Errorf("eqmem modify: resource to insert is missing ID")
		}
	}
	resp := new(entroq.ModifyResponse)
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

	// Get queues that are involved in this modification so we can grab locks.
	// Also find any insertion requests with IDs, where the ID is in a queue
	// different from the one requested. On replay, modPrep also backfills queues
	// that older journals omitted.
	queues, namespaces, misplacedInsIDs, err := m.modPrep(mod, replay)
	if err != nil {
		return nil, fmt.Errorf("modify: %w", err)
	}
	// We can short-circuit if there are no known queues or namespaces to lock.
	if len(queues) == 0 && len(namespaces) == 0 && len(mod.Deletes) == 0 && len(mod.Depends) == 0 {
		return resp, nil
	}

	// Lock all queues and namespaces.
	qls, unlockQueues := m.lockQueues(queues)
	defer unlockQueues()
	nls, unlockNamespaces := m.lockNamespaces(namespaces)
	defer unlockNamespaces()

	byQ := make(map[string]*qLock)
	for _, ql := range qls {
		byQ[ql.queue] = ql
	}
	byNS := make(map[string]*nsLock)
	for _, nl := range nls {
		byNS[nl.namespace] = nl
	}

	// Find the actual tasks and resources involved.
	found := make(map[string]*entroq.Task)
	addFound := func(q string, id string) {
		if q == "" || id == "" {
			return
		}
		if ql, ok := byQ[q]; ok {
			if t, ok := ql.tasks.Get(id); ok {
				found[t.ID] = t
			}
		}
	}

	foundDocs := make(map[string]*entroq.Doc)
	addFoundDoc := func(ns string, id string) {
		if ns == "" || id == "" {
			return
		}
		if nl, ok := byNS[ns]; ok {
			if d, ok := nl.docs.Get(id); ok {
				foundDocs[entroq.DocKey(ns, d.ID)] = d
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
		if q, ok := misplacedInsIDs[t.ID]; ok {
			addFound(q, t.ID)
		}
	}

	for _, d := range mod.DocDeletes {
		addFoundDoc(d.Namespace, d.ID)
	}
	for _, d := range mod.DocDepends {
		addFoundDoc(d.Namespace, d.ID)
	}
	for _, c := range mod.DocChanges {
		addFoundDoc(c.Namespace, c.ID)
	}
	for _, t := range mod.DocInserts {
		addFoundDoc(t.Namespace, t.ID)
	}

	if err := mod.DependencyError(found, foundDocs); err != nil {
		depErr, ok := entroq.AsDependency(err)
		if !replay || !ok || !depErr.OnlyClaims() {
			return nil, fmt.Errorf("eqmem modify: %w", err)
		}
	}

	// Now that we know we can proceed with our process, make all of the necessary changes.
	// We got all of the queue-based stuff handed to us previously, so we
	// already hold all of the locks for that stuff and can edit with impunity.

	var finalLockedSteps []func()

	deleteID := func(q string, id string) {
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

	// Resource operations hold nsLock and need no global-index step (no equivalent
	// of qByID for resources), so they are applied directly rather than deferred.
	setRes := func(r *entroq.Doc) {
		byNS[r.Namespace].docs.Set(r.ID, r)
	}
	deleteResID := func(ns, id string) {
		byNS[ns].docs.Delete(id)
	}

	now, err := m.Time(ctx)
	if err != nil {
		return nil, fmt.Errorf("modify get time: %w", err)
	}

	for _, d := range mod.Deletes {
		deleteID(d.Queue, d.ID)
	}
	for _, c := range mod.Changes {
		newTask := c.Copy()
		newTask.Version++
		// Cap a far-past arrival to now (backend Modify contract): an omitted At
		// arrives now and is ordered at now, not in the distant past.
		newTask.At = entroq.NormalizeArrival(newTask.At, now)
		// Preserve claimant on renewal (At pushed to future); clear otherwise.
		if !newTask.At.After(now) {
			newTask.Claimant = ""
		}
		newTask.Modified = now
		if c.FromQueue != c.Queue {
			deleteID(c.FromQueue, c.ID)
			insertTask(newTask)
		} else {
			// Original version was already checked earlier.
			updateTask(newTask)
		}
		resp.ChangedTasks = append(resp.ChangedTasks, newTask)
	}
	for _, td := range mod.Inserts {
		id := td.ID
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
			At:       entroq.NormalizeArrival(td.At, now),
			Value:    td.Value,
			Claimant: mod.Claimant,
			Created:  created,
			Modified: modified,
		}
		insertTask(newTask)
		resp.InsertedTasks = append(resp.InsertedTasks, newTask)
	}

	for _, d := range mod.DocDeletes {
		deleteResID(d.Namespace, d.ID)
	}
	for _, c := range mod.DocChanges {
		newRes := c.Copy()
		newRes.Version++
		// Cap a far-past arrival to now (backend Modify contract), same as tasks.
		newRes.At = entroq.NormalizeArrival(newRes.At, now)
		// Claim/renew if requested.At is in the future; release otherwise.
		if newRes.At.After(now) {
			newRes.Claimant = mod.Claimant
		} else {
			newRes.Claimant = ""
		}
		newRes.Modified = now
		setRes(newRes)
		resp.ChangedDocs = append(resp.ChangedDocs, newRes)
	}
	for _, rd := range mod.DocInserts {
		id := rd.ID
		created := rd.Created
		if created.IsZero() {
			created = now
		}
		modified := rd.Modified
		if modified.IsZero() {
			modified = now
		}
		newRes := &entroq.Doc{
			Namespace:    rd.Namespace,
			ID:           id,
			Content:      rd.Content,
			Key:          rd.Key,
			SecondaryKey: rd.SecondaryKey,
			Claimant:     "",
			Created:      created,
			Modified:     modified,
			Version:      1,
		}
		setRes(newRes)
		resp.InsertedDocs = append(resp.InsertedDocs, newRes)
	}

	func() {
		defer un(lock(m))
		for _, step := range finalLockedSteps {
			if step != nil {
				step()
			}
		}
	}()

	// Journal the final state of all changes so replay restores exactly what
	// was committed. Inserted tasks/resources carry timestamps via Data() so
	// replay preserves created/modified rather than using replay time.
	// Changes are stored at their final version; the journal player decrements
	// version by one before re-applying so the version-check passes.
	if m.journal != nil {
		jMod := &entroq.Modification{
			Claimant:   mod.Claimant,
			Deletes:    mod.Deletes,
			Depends:    mod.Depends,
			Changes:    resp.ChangedTasks,
			DocDeletes: mod.DocDeletes,
			DocDepends: mod.DocDepends,
			DocChanges: resp.ChangedDocs,
		}
		for _, ins := range resp.InsertedTasks {
			jMod.Inserts = append(jMod.Inserts, ins.Data())
		}
		for _, ins := range resp.InsertedDocs {
			jMod.DocInserts = append(jMod.DocInserts, ins.Data())
		}
		b, err := json.Marshal(jMod)
		if err != nil {
			log.Fatalf("Inconsistent internal state: modification succeeded but could not marshal JSON: %v", err)
		}
		if err := m.journal.Append(b); err != nil {
			log.Fatalf("Inconsistent internal state: modification succeeded but could not append to journal: %v", err)
		}
	}

	entroq.NotifyModified(m.nw, resp.InsertedTasks, resp.ChangedTasks)

	// All done!
	return resp, nil
}

// Time returns the current time.
func (m *EQMem) Time(_ context.Context) (time.Time, error) {
	return entroq.ProcessTime(), nil
}

func (m *EQMem) queueForID(id string) (string, bool) {
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
		if tq.Claimant != "" && tq.Claimant != t.Claimant && t.At.After(now) {
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

	qts.Range(func(_ string, t *entroq.Task) bool {
		if tq.Limit != 0 && tq.Limit <= len(found) {
			return false
		}
		tryAdd(t)
		return true
	})

	return found, nil
}

func matchesQuery(val string, qq *entroq.QueuesQuery) bool {
	if len(qq.MatchPrefix) == 0 && len(qq.MatchExact) == 0 {
		return true
	}
	for _, p := range qq.MatchPrefix {
		if strings.HasPrefix(val, p) {
			return true
		}
	}
	return slices.Contains(qq.MatchExact, val)
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
		if !matchesQuery(q, qq) {
			continue
		}
		if qq.Limit > 0 && len(qs) >= qq.Limit {
			break
		}
		qts, ok := m.queueTasks(q)
		if !ok {
			continue
		}

		stats := &entroq.QueueStat{
			Name: q,
		}
		qts.Range(func(_ string, t *entroq.Task) bool {
			stats.Size++
			if t.At.After(now) {
				if t.Claims > 0 {
					stats.Claimed++
				} else {
					stats.Future++
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
	if m.stopGC != nil {
		m.stopGC()
		<-m.gcDone // wait for the GC loop to exit before tearing down
	}
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
	// Lock in a consistent (sorted) order so concurrent multi-queue modifies can
	// never deadlock (dining philosophers). Sorting here, rather than trusting
	// callers to pre-sort, makes the invariant impossible to get wrong. qs is
	// sorted in place; every caller passes a slice it owns (a fresh
	// single-element slice, or modPrep's result), so reordering it is harmless.
	slices.Sort(qs)
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

func (m *EQMem) locksForNamespaces(ns []string) []*nsLock {
	defer un(lock(m))
	var locks []*nsLock
	for _, n := range ns {
		locks = append(locks, m.lockForNamespaceUnsafe(n))
	}
	return locks
}

func (m *EQMem) lockForNamespaceUnsafe(ns string) (nl *nsLock) {
	defer func() {
		nl.dependents++
	}()

	nl = m.locksSuperUnsafeNS[ns]

	if nss := m.namespaces[ns]; (nss == nil) != (nl == nil) {
		log.Fatalf("Namespace storage and lock structures out of step for namespace %q: nss=%v, nl=%v", ns, nss, nl)
	}

	if nl != nil {
		return nl
	}

	nss := newDocNamespace(ns)
	m.namespaces[ns] = nss

	nl = &nsLock{
		namespace: ns,
		docs:      nss,
	}
	m.locksSuperUnsafeNS[ns] = nl

	return nl
}

func (m *EQMem) lockNamespaces(ns []string) ([]*nsLock, func()) {
	if len(ns) == 0 {
		return nil, func() {}
	}
	// Sorted lock order, same dining-philosophers reasoning as lockQueues. ns is
	// sorted in place; callers own the slice they pass.
	slices.Sort(ns)
	nls := m.locksForNamespaces(ns)
	for _, nl := range nls {
		nl.Lock()
	}

	return nls, func() {
		for i := len(nls) - 1; i >= 0; i-- {
			nls[i].Unlock()
		}
		defer un(lock(m))
		for i := len(nls) - 1; i >= 0; i-- {
			nl := nls[i]
			nl.dependents--

			if nss := m.namespaces[nl.namespace]; nl.dependents == 0 && nss.Len() == 0 {
				delete(m.namespaces, nl.namespace)
				delete(m.locksSuperUnsafeNS, nl.namespace)
			}
		}
	}
}
