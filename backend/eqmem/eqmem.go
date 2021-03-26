// Package eqmem implements an in-memory entroq that has fine-grained locking
// and can handle simultaneously stats/task listing and modifications to a
// large extent.
package eqmem

import (
	"context"
	"sync"
	"time"
)

type EQMem struct {
	sync.Mutex

	// queueTasks is a sync.Map of sync.Maps: map[<queue>]map[<id>]*Task.
	// These are the actual tasks in the system. The rest of the structures
	// are indexes into this map.
	queueTasks *sync.Map

	// queueLocks contains lockers for each known queue.
	queueLocks map[string]sync.Locker

	// modIndex maps from task IDs to enough info to determine whether
	// modifications are allowed, or to search for relevant queues.
	modIndex modMap

	// claimIndex contains information making it easy to search for tasks to
	// claim.
	claimIndex map[string]*claimHeap
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
		queueLocks: make(map[string]sync.Locker),
		modIndex:   makeModMap(),
		claimIndex: make(map[string]*claimHeap),
	}
}

// Time returns the current time.
func (m *EQMem) Time(_ context.Context) (time.Time, error) {
	return entroq.ProcessTime(), nil
}

func (m *EQMem) idInfo(id uuid.UUID) (*modInfo, bool) {
	defer un(lock(m))
	return m.modIndex.info(id)
}

// Tasks lists tasks according to the given query. If specific IDs are given,
// it may block (hopefully briefly) to find queues for them.
func (m *EQMem) Tasks(ctx context.Context, tq *entroq.TasksQuery) ([]*entroq.Task, error) {
	// Plain IDs first.
	now, err := m.Time(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "eqmem tasks time")
	}

	var found []*entroq.Task

	canAdd := func(q string, at time.Time, claimant uuid.UUID) bool {
		if tq.Queue != "" && tq.Queue != q {
			return false
		}
		if tq.Claimant != uuid.Nil && tq.Claimant != claimant && at.After(now) {
			return false
		}
		return true
	}

	// First get the specific IDs ready.
	if len(tq.IDs) != 0 {
		for _, id := range tq.IDs {
			if tq.Limit > 0 && len(found) > tq.Limit {
				break
			}
			inf, ok := m.idQueue(id)
			if ok && canAdd(inf.Queue, inf.At, inf.Claimant) {
				// Now get the actual task and add it, since it's allowed.
				if t, ok := m.queueTasks.Load(inf.Queue); ok {
					found = append(found, t.(*entroq.Task))
				}
			}
		}
		return found, nil
	}

	// No specific IDs, just a queue? Iterate over the queue map itself.
	tasks, ok := m.queueTasks.Load(tq.Queue)
	if !ok {
		return nil, nil // no queue found!
	}

	tasks.Range(func(_, v interface{}) bool {
		if tq.Limit > 0 && len(found) > tq.Limit {
			return false
		}
		task := v.(*entroq.Task).CopyWithValue(!tq.OmitValues)
		found = append(found, task)
	})

	return found, nil
}

// Thought:
//
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
// - Just iterate over queue-task maps.Fine-grained Locks:
//
// Things to lock: heaps, ids, system
// Note: If all relevant heaps are locked, IDs won't be manipulated or searched by something else touching the same queues.
// Note: IDs are only used for dependency checking, but we *also* have queues available.
//
// Claim:
// - Find matching queues, get lock ptrs
// - Shuffle.
// - One by one, lock a queue, search for an available task in it.
// - Change the task's version and a few other tidbits.
//
// Modify:
// - Find matching tasks to satisfy dependency queries (ID map)
// - Get all relevant queues from the list.
// - Manipulate all relevant tasks.
//
// List queue:
// - Find matching queues, get lock ptrs
// - Lock queue for reading, pull all tasks. Repeat
//
// Stats:
// - Find all queue names
// - Lock each queue for reading, compute stats
//
// When a task is manipulated, it affects the ID map, which is a global construct, but it doesn't affect that directly (except on insertion/deletion), it affects the things it points to (for claims, at least).
//
// Note that in every case above, there is a consultation of the "queue list", and after that all locking is done at the queue level. The ID map is manipulated only in Claim and Modify, and is not consulted for task listings or stats.
//
// Task listings actually can list only specific task IDs, though, so in that special case the ID map is accessed. But the lock on that map can be grabbed opportunistically because it will be done under relevant queue locks that ensure that affected tasks are not bothered. However,  there is a rather large problem: the task itself is changing, and returning it during _reads_ is problematic. The one problematic case is when tasks are searched *by ID only*, which is an important, if somewhat corner, case.
//
// Q: how can we carefully manage this so that the ID map is okay to read at all times? Do we want to store actual tasks in a sync.Map and only manipulate them through that structure? In that case, what would be stored in the queue heap would only be enough data (not the whole task) to identify readiness...
//
// What if the tasks themselves were stored in a sync.Map, and only the bare metadata needed to permit manipulation were in the queue heaps? Locking would still be needed, but the actual tasks would only be pointed to from one place instead of two. What would this look like?
//
// - Heaps: entries contain only ID and arrival time (already-claimed tasks can't even be claimed by their own claimants), keyed on queue.
// - Modify Map: entries contain version, queue, claimant, and arrival time, keyed on ID.
//
// In this way, the heap and the map are used _only to *find* tasks and dependencies_. They are truly indices. To get at the actual tasks, we look them up in the sync.Map.
//
// If we look at it this way, it means we take up more space, but it guarantees that something iterating over a queue can safely get the task associated with the ID by getting it from the sync.Map. And that structure never needs to be locked as a unit: any edits being done on it are happening in the indices where true synchronization has to happen anyway.
//
// Looking at it this way, we have
//
// Claim:
// - RLock queue list
// - Get matching queues and lockers
// - Unlock queue list
// - In turn, lock each queue locker, search for a claimable task.
// - Modify the indices and the sync.Map task under ID Map lock. Return it.
//
// Note that this last step is safe because anything else wanting to modify that task would have to hold the same queue lock.
//
// Modify:
// - Lock ID Map
// - Find all dependency task IDs, find all queues and lockers
// - Lock all queues
// - Modify indices and actual tasks in sync.Map.
// - Unlock all queues
// - Unlock ID Map
// - Return collected results.
//
// Note that the ID Map lock is held the entire time in this instance. Modifications don't involve any search, so this is typically a very fast operation. A global lock (such as the ID Map) is suitable for this case.
//
// Tasks:
// - RLock queue list
// - Get matching queues and lockers
// - Unlock queue list
// - In turn, RLock each queue heap, iterate over all tasks, looking up in sync.Map. Unlock.
//
// Stats: same as Tasks, but with stat collection. Can make more efficient over time by using stat caches or a fully ordered collection instead of heaps.
//
// Thought:
//
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
