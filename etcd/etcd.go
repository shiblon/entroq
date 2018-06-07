// Package etcd provides a backend for an entroq.Client using etcd.
//
// This works by creating the following etcd structure:
// 		task/queue/<queue_name> = <num_tasks_in_queue>
// 			.../at/<sortable_timestamp> = <task_uuid>
// 			.../leased/<task_uuid> = <claimant_uuid>
// 		task/data/<task_uuid> = <serialized_task>
//
// The task/queue/<queue_name> counter can also be used as a lock for index
// manipulation, and to delete a queue entry when there are no tasks in it.
//
// The task/queue/<queue_name>/leased/<task_uuid> can be used for notifications
// (watching for a lease to expire, for example, could trigger a claim).
package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/clientv3/concurrency"
	"github.com/google/uuid"
	"github.com/shiblon/entroq"
)

type backend struct {
	cli *clientv3.Client
}

const tsFormat = "2006-01-02T15:04:05.000"

// New creates a new etcd backend for task storage.
func New(client *clientv3.Client) (*backend, error) {
	return &backend{cli: client}, nil
}

// Close closes the underlying client connection.
func (b *backend) Close() error {
	return b.cli.Close()
}

// Queues returns a list of known queues.
func (b *backend) Queues(ctx context.Context) ([]string, error) {
	var queues []string

	// In a loop, we get the queue names from the appropriate path,
	// incrementing to the next sibling "directory" with each step.
	const qdir = "task/queue/"
	next := ""
	for {
		dir := path.Join(qdir, next)
		resp, err := b.cli.Get(ctx, dir,
			clientv3.WithFromKey(),
			clientv3.WithLimit(1),
			clientv3.WithKeysOnly())
		if err != nil {
			return nil, fmt.Errorf("error retrieving from key %q: %v", dir, err)
		}
		if len(resp.Kvs) == 0 {
			break
		}
		key := string(resp.Kvs[0].Key)
		if !strings.HasPrefix(key, qdir) {
			break // all done with what we were looking for.
		}
		queue := strings.TrimPrefix(key, qdir)
		if strings.Contains(queue, "/") {
			next = queue + string('/'+1) // skip subdirs
			continue
		}
		queues = append(queues, queue)
		next = queue + "\x00"
	}
	return queues, nil
}

// Tasks returns a list of tasks in the given queue. If claimant is the zero value, returns all tasks.
func (b *backend) Tasks(ctx context.Context, queue string, claimant uuid.UUID) ([]*entroq.Task, error) {
	qdir := path.Join("task/queue", queue, "at")

	endKey := qdir + string('/'+1)
	if claimant == uuid.Nil {
		endKey = path.Join(qdir, time.Now().UTC().Format(tsFormat)+"\x00")
	}

	idResp, err := b.cli.Get(ctx, path.Join("task/queue", queue, "at/"),
		clientv3.WithRange(endKey),
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend),
	)
	if err != nil {
		return nil, fmt.Errorf("error getting task IDs for queue %q: %v", queue, err)
	}
	var ids []uuid.UUID
	for _, kv := range idResp.Kvs {
		id, err := uuid.ParseBytes(kv.Value)
		if err != nil {
			return nil, fmt.Errorf("error parsing task ID %q: %v", string(kv.Value), err)
		}
		ids = append(ids, id)
	}

	var tasks []*entroq.Task
	for _, id := range ids {
		resp, err := b.cli.Get(ctx, dataKey(id))
		if err != nil {
			return nil, fmt.Errorf("error getting task ID %q: %v", id.String(), err)
		}
		if len(resp.Kvs) == 0 {
			// Skip - not a big deal if there's a read/delete race.
			continue
		}
		kv := resp.Kvs[0]
		t := new(entroq.Task)
		if err := json.Unmarshal(kv.Value, t); err != nil {
			return nil, fmt.Errorf("invalid json in task: %v", err)
		}
		t.Version = int32(kv.Version)
		t.ID = id
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func dataKey(id uuid.UUID) string {
	return path.Join("task/data", id.String())
}

func atBase(id uuid.UUID, t time.Time) string {
	return path.Join(t.Format(tsFormat), id.String())
}

func qAtKey(q string, id uuid.UUID, t time.Time) string {
	return path.Join("task/queue", q, "at", atBase(id, t))
}

func queueCount(stm concurrency.STM, key string) (int, error) {
	val := stm.Get(key)
	if val == "" {
		return 0, nil
	}
	ival, err := strconv.Atoi(val)
	if err != nil {
		return 0, fmt.Errorf("invalid queue count %q: %v", val, err)
	}
	return ival, nil
}

// Modify attempts to make an atomic batch modification to task storage.
func (b *backend) Modify(ctx context.Context, claimant uuid.UUID, mod *entroq.Modification) (inserted []*entroq.Task, changed []*entroq.Task, err error) {
	deps, err := mod.AllDependencies()
	if err != nil {
		return nil, nil, fmt.Errorf("modify failed: %v", err)
	}

	modFunc := func(stm concurrency.STM) error {
		foundTasks := make(map[uuid.UUID]*entroq.Task)
		queueCounts := make(map[string]int)

		updateQueueCount := func(qName string) error {
			if _, ok := queueCounts[qName]; ok {
				return nil
			}
			val := stm.Get(path.Join("task/queue", qName))
			if val == "" {
				val = "0"
			}
			qc, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("invalid queue count %q: %v", val, err)
			}
			queueCounts[qName] = qc
			return nil
		}

		// In this loop, we get relevant values from
		// - task/queue
		// - task/queue/<queue_name>/at/<eta>/<taskID>
		// - task/data/<taskID>
		//
		// This should serve to ensure that we can update all of these things
		// safely within this transaction.
		for id := range deps {
			// Get the task from the data section.
			val := stm.Get(dataKey(id))
			t := new(entroq.Task)
			if err := json.Unmarshal([]byte(val), t); err != nil {
				return fmt.Errorf("parse error for task: %v", err)
			}
			foundTasks[id] = t

			key := qAtKey(t.Queue, t.ID, t.At)

			// Will fail if it isn't there, which is what we want.
			if atID := stm.Get(key); atID != t.ID.String() {
				return fmt.Errorf("key %q wanted %q, got %q", key, t.ID, atID)
			}

			// Ensure we have the queue count available for changing later.
			if err := updateQueueCount(t.Queue); err != nil {
				return err
			}
		}

		// Before proceeding, make sure we've touched all potential *new* queues
		// (insertions and updates).
		for _, t := range mod.Changes {
			if err := updateQueueCount(t.Queue); err != nil {
				return err
			}
		}
		for _, t := range mod.Inserts {
			if err := updateQueueCount(t.Queue); err != nil {
				return err
			}
		}

		// Check for bad versions, deletes, etc.
		if err := mod.DependencyError(foundTasks); err != nil {
			return err
		}

		// Now that we have applied all possible 'Get's, do the actual modifications.
		now := time.Now().UTC()

		// Insert new tasks.
		for _, td := range mod.Inserts {
			task := &entroq.Task{
				ID:       uuid.New(),
				Version:  0,
				Claimant: claimant,

				Queue: td.Queue,
				At:    td.At,
				Value: td.Value,

				Created:  now,
				Modified: now,
			}
			if task.At.IsZero() {
				task.At = now
			}
			taskBytes, err := json.Marshal(task)
			if err != nil {
				return fmt.Errorf("insert failed to serialize: %v", err)
			}
			stm.Put(dataKey(task.ID), string(taskBytes))
			stm.Put(qAtKey(task.Queue, task.ID, task.At), task.ID.String())
			queueCounts[task.Queue]++

			inserted = append(inserted, task)
		}

		// Do deletions
		for _, tid := range mod.Deletes {
			task := foundTasks[tid.ID]
			stm.Del(dataKey(task.ID))
			stm.Del(qAtKey(task.Queue, task.ID, task.At))
			queueCounts[task.Queue]--
		}

		// Do updates.
		for _, t := range mod.Changes {
			task := &entroq.Task{
				ID:      t.ID,
				Queue:   t.Queue,
				At:      t.At,
				Value:   t.Value,
				Created: t.Created,

				Version:  t.Version + 1,
				Claimant: claimant,
				Modified: now,
			}
			taskBytes, err := json.Marshal(task)
			if err != nil {
				return fmt.Errorf("change failed to serialize: %v", err)
			}
			stm.Put(dataKey(task.ID), string(taskBytes))

			found := foundTasks[t.ID]

			// If the ETA or queue changed, update the queue index.
			if found.At != task.At || found.Queue != task.Queue {
				stm.Del(qAtKey(found.Queue, found.ID, found.At))
				stm.Put(qAtKey(task.Queue, task.ID, task.At), task.ID.String())
			}

			// No-op if queue didn't change, otherwise moves counts.
			queueCounts[found.Queue]--
			queueCounts[task.Queue]++

			changed = append(changed, task)
		}

		// Finally, update all queue counts.
		//
		// Note that the only way we can get a 0 queue count is by moving
		// from one queue to another or deleting from a queue. That means
		// we won't ever get a zero count just because a queue didn't exist
		// when we started (e.g., for an insert or the *destination* of a
		// change). Thus, it's safe to delete those queues
		for q, c := range queueCounts {
			qkey := path.Join("task/queue", q)
			switch {
			case c == 0:
				stm.Del(qkey)
			default:
				stm.Put(qkey, strconv.Itoa(c))
			}
		}

		return nil
	}
	stm, err := concurrency.NewSTM(b.cli, modFunc)
	if err != nil {
		return nil, nil, fmt.Errorf("error in modify stm: %v", err)
	} else if !stm.Succeeded {
		return nil, nil, fmt.Errorf("failed modify: %#v", stm.Responses)
	}

	return inserted, changed, nil
}

// TryClaim attempts to claim an unclaimed and expired task from the given queue.
func (b *backend) TryClaim(ctx context.Context, queue string, claimant uuid.UUID, duration time.Duration) (*entroq.Task, error) {
	now := time.Now().UTC()

	resp, err := b.cli.Get(ctx, qAtKey(queue, uuid.Nil, time.Time{}),
		clientv3.WithRange(qAtKey(queue, uuid.Nil, now)),
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend),
	)
	if err != nil {
		return nil, fmt.Errorf("error getting claimable tasks: %v", err)
	}
	// None to claim, not an error.
	if len(resp.Kvs) == 0 {
		return nil, nil
	}

	// Get all of the IDs and keys so that when we find one, we can update its ETA.
	qKeyByID := make(map[uuid.UUID]string)
	var toTry []uuid.UUID
	for _, kv := range resp.Kvs {
		id, err := uuid.ParseBytes(kv.Value)
		if err != nil {
			return nil, fmt.Errorf("error parsing At ID: %v", err)
		}
		qKeyByID[id] = string(kv.Key)
		toTry = append(toTry, id)
	}

	var tryID uuid.UUID

	var claimedTask *entroq.Task
	claimFunc := func(stm concurrency.STM) error {
		// Try to claim the tryID task (and get its corresponding At index).
		// Fail if we can't do it. We'll retry from the outside.
		qkey, ok := qKeyByID[tryID]
		if !ok {
			return fmt.Errorf("no queue key %q after finding %q there", qkey, tryID)
		}
		tstr := stm.Get(dataKey(tryID))
		task := new(entroq.Task)
		if err := json.Unmarshal([]byte(tstr), task); err != nil {
			return fmt.Errorf("json unmarshal failed for task %q: %v", tryID, err)
		}
		task.At = now.Add(duration)
		task.Modified = now
		task.Claimant = claimant

		tbytes, err := json.Marshal(task)
		if err != nil {
			return fmt.Errorf("json marshal of claimed task failed: %v", err)
		}
		stm.Put(dataKey(tryID), string(tbytes))
		stm.Del(qkey)
		stm.Put(qAtKey(queue, task.ID, task.At), task.ID.String())

		// Pass info out. We succeeded.
		claimedTask = task
		return nil
	}

	// Try all keys in sorted order until we find one claimable or run out.
	//
	// It's okay if new ones appear while we're doing this (and we miss them
	// because of the inherent race in listing + individual STMs). Retries are
	// part of the task acquisition. We're just trying to *limit* retries, not
	// *eliminate* them.
	for _, id := range toTry {
		// Set up the input to the STM function.
		tryID = id

		stm, err := concurrency.NewSTM(b.cli, claimFunc)
		if err != nil {
			return nil, fmt.Errorf("error in claim stm: %v", err)
		}
		if stm.Succeeded {
			// Pass out the output from the STM function.
			return claimedTask, nil
		}
	}

	// Didn't find any claimable tasks.
	return nil, nil
}
