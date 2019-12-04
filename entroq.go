// Copyright 2019 Chris Monson <shiblon@gmail.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Package entroq contains the main task queue client and data definitions. The
// client relies on a backend to implement the actual transactional
// functionality, the interface for which is also defined here.
package entroq // import "entrogo.com/entroq"

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	DefaultClaimPollTime = 30 * time.Second
	DefaultClaimDuration = 30 * time.Second
)

// TaskID contains the identifying parts of a task. If IDs don't match
// (identifier and version together), then operations fail on those tasks.
type TaskID struct {
	ID      uuid.UUID
	Version int32
}

// String produces the id:version string representation.
func (t TaskID) String() string {
	return fmt.Sprintf("%s:v%d", t.ID, t.Version)
}

// AsDeletion produces an appropriate ModifyArg to delete the task with this ID.
func (t TaskID) AsDeletion() ModifyArg {
	return Deleting(t.ID, t.Version)
}

// TaskData contains just the data, not the identifier or metadata. Used for insertions.
type TaskData struct {
	Queue string
	At    time.Time
	Value []byte

	// ID is an optional task ID to be used for task insertion.
	// Default (uuid.Nil) causes the backend to assign one, and that is
	// sufficient for many cases. If you desire to make a database entry that
	// *references* a task, however, in that case it can make sense to specify
	// an explicit task ID for insertion. This allows a common workflow cycle
	//
	// 	consume task -> db update -> insert tasks
	//
	// to be done safely, where the database update needs to refer to
	// to-be-inserted tasks.
	ID uuid.UUID

	// skipCollidingID indicates that a collision on insertion is not fatal,
	// and the insertion can be removed if that happens, and then the
	// modification can be retried.
	skipCollidingID bool
}

// String returns a string representation of the task data, excluding the value.
func (t *TaskData) String() string {
	s := fmt.Sprintf("%q::%v", t.Queue, t.At)
	if t.ID != uuid.Nil {
		s += "::" + t.ID.String()
	}
	return s
}

// Task represents a unit of work, with a byte slice value payload.
// Note that Claims is the number of times a task has successfully been claimed.
// This is different than the version number, which increments for
// every modification, not just claims.
type Task struct {
	Queue string `json:"queue"`

	ID      uuid.UUID `json:"id"`
	Version int32     `json:"version"`

	At       time.Time `json:"at"`
	Claimant uuid.UUID `json:"claimant"`
	Claims   int32     `json:"claims"`
	Value    []byte    `json:"value"`

	Created  time.Time `json:"created"`
	Modified time.Time `json:"modified"`
}

// String returns a useful representation of this task.
func (t *Task) String() string {
	return fmt.Sprintf("Task [%q %s v%d]\n\t", t.Queue, t.ID, t.Version) + strings.Join([]string{
		fmt.Sprintf("at=%q claimant=%s", t.At, t.Claimant),
		fmt.Sprintf("c=%q m=%q", t.Created, t.Modified),
		fmt.Sprintf("val=%q", string(t.Value)),
	}, "\n\t") + "\n"
}

// AsDeletion returns a ModifyArg that can be used in the Modify function, e.g.,
//
//   cli.Modify(ctx, task1.AsDeletion())
//
// The above would cause the given task to be deleted, if it can be. It is
// shorthand for
//
//   cli.Modify(ctx, Deleting(task1.ID(), task1.Version()))
func (t *Task) AsDeletion() ModifyArg {
	return Deleting(t.ID, t.Version)
}

// AsChange returns a ModifyArg that can be used in the Modify function, e.g.,
//
//   cli.Modify(ctx, task1.AsChange(ArrivalTimeBy(2 * time.Minute)))
//
// The above is shorthand for
//
//   cli.Modify(ctx, Changing(task1, ArrivalTimeBy(2 * time.Minute)))
func (t *Task) AsChange(args ...ChangeArg) ModifyArg {
	return Changing(t, args...)
}

// AsDependency returns a ModifyArg that can be used to create a Modify dependency, e.g.,
//
//   cli.Modify(ctx, task.AsDependency())
//
// That is shorthand for
//
//   cli.Modify(ctx, DependingOn(task.ID(), task.Version()))
func (t *Task) AsDependency() ModifyArg {
	return DependingOn(t.ID, t.Version)
}

// ID returns a Task ID from this task.
func (t *Task) IDVersion() *TaskID {
	return &TaskID{
		ID:      t.ID,
		Version: t.Version,
	}
}

// Data returns the data for this task.
func (t *Task) Data() *TaskData {
	return &TaskData{
		Queue: t.Queue,
		At:    t.At,
		Value: t.Value,
		ID:    t.ID,
	}
}

// Copy copies this task's data and everything.
func (t *Task) Copy() *Task {
	newT := new(Task)
	*newT = *t
	newT.Value = make([]byte, len(t.Value))
	copy(newT.Value, t.Value)
	return newT
}

// ClaimQuery contains information necessary to attempt to make a claim on a task in a specific queue.
type ClaimQuery struct {
	Queues   []string      // Queues to attempt to claim from. Only one wins.
	Claimant uuid.UUID     // The ID of the process trying to make the claim.
	Duration time.Duration // How long the task should be claimed for if successful.
	PollTime time.Duration // Length of time between (possibly interruptible) sleep and polling.
}

// TasksQuery holds information for a tasks query.
type TasksQuery struct {
	Queue    string
	Claimant uuid.UUID
	Limit    int
	IDs      []uuid.UUID
}

// QueuesQuery modifies a queue listing request.
type QueuesQuery struct {
	// MatchPrefix specifies allowable prefix matches. If empty, limitations
	// are not set based on prefix matching. All prefix match conditions are ORed.
	// If both this and MatchExact are empty or nil, no limitations are set on
	// queue name: all will be returned.
	MatchPrefix []string

	// MatchExact specifies allowable exact matches. If empty, limitations are
	// not set on exact queue names.
	MatchExact []string

	// Limit specifies an upper bound on the number of queue names returned.
	Limit int
}

// QueueStat holds high-level information about a queue.
// Note that available + claimed may not add up to size. This is because a task
// can be unavailable (AT in the future) without being claimed by anyone.
type QueueStat struct {
	Name      string `json:"name"`      // The queue name.
	Size      int    `json:"size"`      // The total number of tasks.
	Claimed   int    `json:"claimed"`   // The number of currently claimed tasks.
	Available int    `json:"available"` // The number of available tasks.
}

// QueuesFromStats can be used for converting the new QueueStats to the old
// Queues output, making it easier on backend implementers to just define one
// function (similar to how WaitTryClaim or PollTryClaim can make implementing
// Claim in terms of TryClaim easier).
func QueuesFromStats(stats map[string]*QueueStat, err error) (map[string]int, error) {
	if err != nil {
		return nil, err
	}
	qs := make(map[string]int)
	for k, v := range stats {
		qs[k] = v.Size
	}
	return qs, nil
}

// BackendClaimFunc is a function that can make claims based on a ClaimQuery.
// It is a convenience type for backends to use.
type BackendClaimFunc func(ctx context.Context, eq *ClaimQuery) (*Task, error)

// PollTryClaim runs a loop in which the TryClaim function is called between
// sleeps with exponential backoff (up to a point). Backend implementations may
// choose to use this as their Claim implementation.
func PollTryClaim(ctx context.Context, eq *ClaimQuery, tc BackendClaimFunc) (*Task, error) {
	const (
		maxCheckInterval = 30 * time.Second
		startInterval    = time.Second
	)

	curWait := startInterval
	for {
		// Don't wait longer than the check interval or canceled context.
		task, err := tc(ctx, eq)
		if err != nil {
			return nil, errors.Wrap(err, "poll try claim")
		}
		if task != nil {
			return task, nil
		}
		// No error, no task - we wait with exponential backoff and try again.
		select {
		case <-time.After(curWait):
			curWait *= 2
			if curWait > maxCheckInterval {
				curWait = maxCheckInterval
			}
		case <-ctx.Done():
			return nil, errors.Wrap(ctx.Err(), "poll try claim")
		}
	}
}

// Waiter can wait for an event on a given key (e.g., queue name).
type Waiter interface {
	// Wait waits for an event on the given set of keys, calling cond after
	// poll intervals until one of them is notified, cond returns true, or the
	// context is canceled.
	//
	// If cond is nil, this function returns when the channel is notified,
	// the poll interval is exceeded, or the context is canceled. Only the last
	// event causes a non-nil error.
	//
	// If poll is 0, it can never be exceeded.
	//
	// A common use is to use poll=0 and cond=nil, causing this to simply wait
	// for a notification.
	Wait(ctx context.Context, keys []string, poll time.Duration, cond func() bool) error
}

// Notifier can be notified on a given key (e.g., queue name);
type Notifier interface {
	// Notify signals an event on the key. Wakes up one waiter, if any, or is
	// dropped if no waiters are waiting.
	Notify(key string)
}

// NotifyWaiter can wait for and notify events.
type NotifyWaiter interface {
	Notifier
	Waiter
}

// NotifyModified takes inserted and changed tasks and notifies once per unique queue/ID pair.
func NotifyModified(n Notifier, inserted, changed []*Task) {
	now := time.Now()
	qs := make(map[uuid.UUID]string)
	for _, t := range inserted {
		if !now.Before(t.At) {
			qs[t.ID] = t.Queue
		}
	}
	for _, t := range changed {
		if !now.Before(t.At) {
			qs[t.ID] = t.Queue
		}
	}
	for _, q := range qs {
		n.Notify(q)
	}
}

// WaitTryClaim runs a loop in which the TryClaim function is called, then if
// no tasks are available, the given wait function is used to attempt to wait
// for a task to become available on the queue.
//
// The wait function should exit (more or less) immediately if the context is
// canceled, and should return a nil error if the wait was successful
// (something became available).
func WaitTryClaim(ctx context.Context, eq *ClaimQuery, tc BackendClaimFunc, w Waiter) (*Task, error) {
	var (
		task    *Task
		condErr error
	)
	pollTime := eq.PollTime
	if pollTime == 0 {
		pollTime = DefaultClaimPollTime
	}
	if err := w.Wait(ctx, eq.Queues, pollTime, func() bool {
		// If we get either a task or an error, time to stop trying.
		task, condErr = tc(ctx, eq)
		return task != nil || condErr != nil
	}); err != nil {
		return nil, errors.Wrap(err, "wait try claim")
	}
	if condErr != nil {
		return nil, errors.Wrap(condErr, "wait try claim condition")
	}
	return task, nil
}

// Backend describes all of the functions that any backend has to implement
// to be used as the storage for task queues.
type Backend interface {
	// Queues returns a mapping from all known queues to their task counts.
	Queues(ctx context.Context, qq *QueuesQuery) (map[string]int, error)

	// QueueStats returns statistics for the specified queues query. Richer
	// than just calling Queues, as it can return more than just the size.
	QueueStats(ctx context.Context, qq *QueuesQuery) (map[string]*QueueStat, error)

	// Tasks retrieves all tasks from the given queue. If claimantID is
	// specified (non-zero), limits those tasks to those that are either
	// expired or belong to the given claimant. Otherwise returns them all.
	Tasks(ctx context.Context, tq *TasksQuery) ([]*Task, error)

	// TryClaim attempts to claim a task from the "top" (or close to it) of the
	// given queue. When claimed, a task is held for the duration specified
	// from the time of the claim. If claiming until a specific wall-clock time
	// is desired, the task should be immediately modified after it is claimed
	// to set At to a specific time. Returns a nil task and a nil error if
	// there is nothing to claim. Will fail with a DependencyError is a
	// specific task ID is requested but not present.
	TryClaim(ctx context.Context, cq *ClaimQuery) (*Task, error)

	// Claim is a blocking version of TryClaim, attempting to claim a task
	// from a queue, and blocking until canceled or a task becomes available.
	//
	// Will fail with a DependencyError is a specific task ID is requested but
	// not present. Never returns both a nil task and a nil error
	// simultaneously: a failure to claim a task is an error (potentially just
	// a timeout).
	Claim(ctx context.Context, cq *ClaimQuery) (*Task, error)

	// Modify attempts to atomically modify the task store, and only succeeds
	// if all dependencies are available and all mutations are either expired
	// or already owned by this claimant. The Modification type provides useful
	// functions for determining whether dependencies are good or bad. This
	// function is intended to return a DependencyError if the transaction could
	// not proceed because dependencies were missing or already claimed (and
	// not expired) by another claimant.
	Modify(ctx context.Context, mod *Modification) (inserted []*Task, changed []*Task, err error)

	// Time returns the time as the backend understands it, in UTC.
	Time(ctx context.Context) (time.Time, error)

	// Close closes any underlying connections. The backend is expected to take
	// ownership of all such connections, so this cleans them up.
	Close() error
}

// EntroQ is a client interface for accessing the task queue.
type EntroQ struct {
	backend  Backend
	clientID uuid.UUID
}

// Option is an option that modifies how EntroQ clients are created.
type Option func(*EntroQ)

// WithClaimantID sets the default claimaint ID for this client.
func WithClaimantID(id uuid.UUID) Option {
	return func(eq *EntroQ) {
		eq.clientID = id
	}
}

// BackendOpener is a function that can open a connection to a backend. Creating
// a client with a specific backend is accomplished by passing one of these functions
// into New.
type BackendOpener func(ctx context.Context) (Backend, error)

// New creates a new task client with the given backend implementation.
//
//   cli := New(ctx, backend)
func New(ctx context.Context, opener BackendOpener, opts ...Option) (*EntroQ, error) {
	backend, err := opener(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "backend connection")
	}
	eq := &EntroQ{
		clientID: uuid.New(),
		backend:  backend,
	}
	for _, o := range opts {
		o(eq)
	}
	return eq, nil
}

// Close closes the underlying backend.
func (c *EntroQ) Close() error {
	return c.backend.Close()
}

// ID returns the default claimant ID of this client. Used in "bare" calls,
// like Modify, Claim, etc. To change the ID per call (usually not needed, and
// can be dangerous), use the "As" calls, e.g., ModifyAs.
func (c *EntroQ) ID() uuid.UUID {
	return c.clientID
}

// Queues returns a mapping from all queue names to their task counts.
func (c *EntroQ) Queues(ctx context.Context, opts ...QueuesOpt) (map[string]int, error) {
	query := new(QueuesQuery)
	for _, opt := range opts {
		opt(query)
	}
	return c.backend.Queues(ctx, query)
}

// QueueStats returns a mapping from queue names to task stats.
func (c *EntroQ) QueueStats(ctx context.Context, opts ...QueuesOpt) (map[string]*QueueStat, error) {
	query := new(QueuesQuery)
	for _, opt := range opts {
		opt(query)
	}
	return c.backend.QueueStats(ctx, query)
}

// QueuesEmpty indicates whether the specified task queues are all empty. If no
// options are specified, returns an error.
func (c *EntroQ) QueuesEmpty(ctx context.Context, opts ...QueuesOpt) (bool, error) {
	if len(opts) == 0 {
		return false, errors.New("empty check: no queue options specified")
	}
	qs, err := c.Queues(ctx, opts...)
	if err != nil {
		return false, errors.Wrap(err, "empty check")
	}
	for _, size := range qs {
		if size > 0 {
			return false, nil
		}
	}
	return true, nil
}

// WaitQueuesEmpty does a poll-and-wait strategy to block until the queue query returns empty.
func (c *EntroQ) WaitQueuesEmpty(ctx context.Context, opts ...QueuesOpt) error {
	for {
		empty, err := c.QueuesEmpty(ctx, opts...)
		if err != nil {
			return errors.Wrap(err, "wait empty")
		}
		if empty {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Wrap(ctx.Err(), "wait empty")
		case <-time.After(2 * time.Second):
		}
	}
}

// Tasks returns a slice of all tasks in the given queue.
func (c *EntroQ) Tasks(ctx context.Context, queue string, opts ...TasksOpt) ([]*Task, error) {
	query := &TasksQuery{
		Queue: queue,
	}
	for _, opt := range opts {
		opt(c, query)
	}

	return c.backend.Tasks(ctx, query)
}

// TasksOpt is an option that can be passed into Tasks to control what it returns.
type TasksOpt func(*EntroQ, *TasksQuery)

// LimitSelf only returns self-claimed tasks or expired tasks.
func LimitSelf() TasksOpt {
	return func(c *EntroQ, q *TasksQuery) {
		q.Claimant = c.clientID
	}
}

// LimitClaimant only returns tasks with the given claimant, or expired tasks.
func LimitClaimant(id uuid.UUID) TasksOpt {
	return func(_ *EntroQ, q *TasksQuery) {
		q.Claimant = id
	}
}

// LimitTasks sets the limit on the number of tasks to return. A value <= 0 indicates "no limit".
func LimitTasks(limit int) TasksOpt {
	return func(_ *EntroQ, q *TasksQuery) {
		q.Limit = limit
	}
}

// WithTaskID adds a task ID to the set of IDs that can be returned in a task
// query. The default is "all that match other specs" if no IDs are specified.
// Note that versions are not part of the ID.
func WithTaskID(ids ...uuid.UUID) TasksOpt {
	return func(_ *EntroQ, q *TasksQuery) {
		q.IDs = append(q.IDs, ids...)
	}
}

// QueuesOpt modifies how queue requests are made.
type QueuesOpt func(*QueuesQuery)

// MatchPrefix adds allowable prefix matches for a queue listing.
func MatchPrefix(prefixes ...string) QueuesOpt {
	return func(q *QueuesQuery) {
		q.MatchPrefix = append(q.MatchPrefix, prefixes...)
	}
}

// MatchExact adds an allowable exact match for a queue listing.
func MatchExact(matches ...string) QueuesOpt {
	return func(q *QueuesQuery) {
		q.MatchExact = append(q.MatchExact, matches...)
	}
}

// LimitQueues sets the limit on the number of queues that are returned.
func LimitQueues(limit int) QueuesOpt {
	return func(q *QueuesQuery) {
		q.Limit = limit
	}
}

// ClaimOpt modifies limits on a task claim.
type ClaimOpt func(*ClaimQuery)

// ClaimAs sets the claimant ID for a claim operation. When not set, uses the internal default for this client.
func ClaimAs(id uuid.UUID) ClaimOpt {
	return func(q *ClaimQuery) {
		q.Claimant = id
	}
}

// ClaimPollTime sets the polling time for a claim. Set to DefaultClaimPollTime if left at 0.
func ClaimPollTime(d time.Duration) ClaimOpt {
	return func(q *ClaimQuery) {
		q.PollTime = d
	}
}

// ClaimFor sets the duration of a successful claim (the amount of time from now when it expires).
func ClaimFor(duration time.Duration) ClaimOpt {
	return func(q *ClaimQuery) {
		q.Duration = duration
	}
}

// From sets the queue(s) for a claim.
func From(qs ...string) ClaimOpt {
	return func(cq *ClaimQuery) {
		// Add and remove duplicates.
		qmap := make(map[string]bool)
		for _, q := range qs {
			qmap[q] = true
		}
		for _, q := range cq.Queues {
			qmap[q] = true
		}
		cq.Queues = make([]string, 0, len(qmap))
		for q := range qmap {
			cq.Queues = append(cq.Queues, q)
		}
	}
}

// IsCanceled indicates whether the error is a canceled error.
func IsCanceled(err error) bool {
	if err == nil {
		return false
	}
	err = errors.Cause(err)
	if err == context.Canceled {
		return true
	}

	if status.Code(err) == codes.Canceled {
		return true
	}

	return false
}

// IsTimeout indicates whether the error is a timeout error.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	err = errors.Cause(err)
	if err == context.DeadlineExceeded {
		return true
	}

	if status.Code(err) == codes.DeadlineExceeded {
		return true
	}

	return false
}

// claimQueryFromOpts processes ClaimOpt values and produces a claim query.
func claimQueryFromOpts(claimant uuid.UUID, opts ...ClaimOpt) *ClaimQuery {
	query := &ClaimQuery{
		Claimant: claimant,
		Duration: DefaultClaimDuration,
		PollTime: DefaultClaimPollTime,
	}
	for _, opt := range opts {
		opt(query)
	}
	return query
}

// Claim attempts to get the next unclaimed task from the given queues. It
// blocks until one becomes available or until the context is done. When it
// succeeds, it returns a task with the claimant set to the default, or to the
// value given in options, and an arrival time computed from the duration. The
// default duration if none is given is DefaultClaimDuration.
func (c *EntroQ) Claim(ctx context.Context, opts ...ClaimOpt) (*Task, error) {
	query := claimQueryFromOpts(c.clientID, opts...)
	if len(query.Queues) == 0 {
		return nil, errors.New("no queues specified for claim")
	}
	return c.backend.Claim(ctx, query)
}

// TryClaim attempts one time to claim a task from the given queues. If
// there are no tasks, it returns a nil error *and* a nil task. This allows the
// caller to decide whether to retry. It can fail if certain (optional)
// dependency tasks are not present. This can be used, for example, to ensure
// that configuration tasks haven't changed.
func (c *EntroQ) TryClaim(ctx context.Context, opts ...ClaimOpt) (*Task, error) {
	query := claimQueryFromOpts(c.clientID, opts...)
	if len(query.Queues) == 0 {
		return nil, errors.New("no queues specified for try claim")
	}
	return c.backend.TryClaim(ctx, query)
}

// RenewFor attempts to renew the given task's lease (update arrival time) for
// the given duration. Returns the new task.
func (c *EntroQ) RenewFor(ctx context.Context, task *Task, duration time.Duration) (*Task, error) {
	changed, err := c.RenewAllFor(ctx, []*Task{task}, duration)
	if err != nil {
		return nil, errors.Wrap(err, "renew task")
	}
	return changed[0], nil
}

// RenewAllFor attempts to renew all given tasks' leases (update arrival times)
// for the given duration. Returns the new tasks.
func (c *EntroQ) RenewAllFor(ctx context.Context, tasks []*Task, duration time.Duration) ([]*Task, error) {
	if len(tasks) == 0 {
		return nil, nil
	}
	// Note that we use the default claimant always in this situation. These
	// functions are high-level user-friendly things and don't allow some of
	// the other hocus pocus that might happen in a grpc proxy situation (where
	// claimant IDs need to come from the request, not the client it uses to
	// perform its work).
	var modArgs []ModifyArg
	var taskIDs []string
	for _, t := range tasks {
		modArgs = append(modArgs, Changing(t, ArrivalTimeBy(duration)))
		taskIDs = append(taskIDs, t.IDVersion().String())
	}
	log.Printf("Renewing tasks %v", taskIDs)
	_, changed, err := c.Modify(ctx, modArgs...)
	if err != nil {
		return nil, errors.Wrapf(err, "renewal failed for tasks %q", taskIDs)
	}
	if len(changed) != len(tasks) {
		return nil, errors.Errorf("renewal expected %d updated tasks, got %d", len(tasks), len(changed))
	}
	return changed, nil
}

// DoWithRenewAll runs the provided function while keeping all given tasks leases renewed.
func (c *EntroQ) DoWithRenewAll(ctx context.Context, tasks []*Task, lease time.Duration, f func(context.Context) error) ([]*Task, error) {
	g, ctx := errgroup.WithContext(ctx)

	doneCh := make(chan struct{})

	renewed := tasks
	g.Go(func() error {
		for {
			select {
			case <-doneCh:
				return nil
			case <-ctx.Done():
				return errors.Wrap(ctx.Err(), "renew all stopped")
			case <-time.After(lease / 2):
				var err error
				if renewed, err = c.RenewAllFor(ctx, renewed, lease); err != nil {
					return errors.Wrap(err, "could not extend lease")
				}
			}
		}
	})

	g.Go(func() error {
		defer close(doneCh)
		return f(ctx)
	})

	if err := g.Wait(); err != nil {
		return nil, errors.Wrap(err, "error running with renewal")
	}

	// The task will have been overwritten with every renewal. Return final task.
	return renewed, nil
}

// DoWithRenew runs the provided function while keeping the given task lease renewed.
func (c *EntroQ) DoWithRenew(ctx context.Context, task *Task, lease time.Duration, f func(context.Context) error) (*Task, error) {
	finalTasks, err := c.DoWithRenewAll(ctx, []*Task{task}, lease, f)
	if err != nil {
		return nil, errors.Wrap(err, "with renew single task")
	}
	return finalTasks[0], nil
}

// Modify allows a batch modification operation to be done, gated on the
// existence of all task IDs and versions specified. Deletions, Updates, and
// Dependencies must be present. The transaction all fails or all succeeds.
//
// Returns all inserted task IDs, and an error if it could not proceed. If the error
// was due to missing dependencies, a *DependencyError is returned, which can be checked for
// by calling AsDependency(err).
func (c *EntroQ) Modify(ctx context.Context, modArgs ...ModifyArg) (inserted []*Task, changed []*Task, err error) {
	mod := NewModification(c.clientID, modArgs...)
	for {
		ins, chg, err := c.backend.Modify(ctx, mod)
		if err == nil {
			return ins, chg, nil
		}
		depErr, ok := AsDependency(err)
		// If not a dependency error, pass it on out.
		if !ok {
			return nil, nil, errors.Wrap(err, "modify")
		}
		// If anything is missing or there's a claim problem, bail.
		if depErr.HasMissing() || depErr.HasClaims() {
			return nil, nil, errors.Wrap(err, "modify non-ins deps")
		}
		// No collisions? Not sure what's going on.
		if !depErr.HasCollisions() {
			return nil, nil, errors.Wrap(err, "non-collision errors")
		}

		// If we get this far, the only errors we have are insertion collisions.
		// If any of them cannot be skipped, we bail out.
		// Otherwise we remove them and try again.

		// Now check all collisions. If we can remove all of them safely, do so
		// and try again.
		collidingIDs := make(map[uuid.UUID]bool)
		for _, ins := range depErr.Inserts {
			collidingIDs[ins.ID] = true
		}
		var newInserts []*TaskData
		for _, td := range mod.Inserts {
			if collidingIDs[td.ID] {
				if td.skipCollidingID {
					// Skippable, so skip.
					continue
				}
				// Can't skip this one. Bail.
				return nil, nil, errors.Wrap(err, "unskippable collision")
			}
			newInserts = append(newInserts, td)
		}
		log.Printf("Trying modification again due to skippable colliding inserts. %v -> %v", mod.Inserts, newInserts)
		mod.Inserts = newInserts
	}
	return c.backend.Modify(ctx, NewModification(c.clientID, modArgs...))
}

// ModifyArg is an argument to the Modify function, which does batch modifications to the task store.
type ModifyArg func(m *Modification)

// ModifyAs sets the claimant ID for a particular modify call. Usually not
// needed, can be dangerous unless used with extreme care. The client default
// is used if this option is not provided.
func ModifyAs(id uuid.UUID) ModifyArg {
	return func(m *Modification) {
		m.Claimant = id
	}
}

// Inserting creates an insert modification from TaskData:
//
// 	cli.Modify(ctx,
// 		Inserting(&TaskData{
// 			Queue: "myqueue",
// 			At:    time.Now.Add(1 * time.Minute),
// 			Value: []byte("hi there"),
// 		}))
//
// Or, better still,
//
//	cli.Modify(ctx,
//		InsertingInto("myqueue",
//		    WithArrivalTimeIn(1 * time.Minute),
//		    WithValue([]byte("hi there"))))
func Inserting(tds ...*TaskData) ModifyArg {
	return func(m *Modification) {
		m.Inserts = append(m.Inserts, tds...)
	}
}

// InsertingInto creates an insert modification. Use like this:
//
//   cli.Modify(InsertingInto("my queue name", WithValue([]byte("hi there"))))
func InsertingInto(q string, insertArgs ...InsertArg) ModifyArg {
	return func(m *Modification) {
		data := &TaskData{Queue: q}
		for _, arg := range insertArgs {
			arg(m, data)
		}
		m.Inserts = append(m.Inserts, data)
	}
}

// InsertArg is an argument to task insertion.
type InsertArg func(*Modification, *TaskData)

// WithArrivalTime changes the arrival time to a fixed moment during task insertion.
func WithArrivalTime(at time.Time) InsertArg {
	return func(_ *Modification, d *TaskData) {
		d.At = at
	}
}

// WithArrivalTimeIn computes the arrival time based on the duration from now, e.g.,
//
//   cli.Modify(ctx,
//     InsertingInto("my queue",
//       WithTimeIn(2 * time.Minute)))
func WithArrivalTimeIn(duration time.Duration) InsertArg {
	return func(m *Modification, d *TaskData) {
		d.At = m.now.Add(duration)
	}
}

// WithValue sets the task's byte slice value during insertion.
//   cli.Modify(ctx,
//     InsertingInto("my queue",
//       WithValue([]byte("hi there"))))
func WithValue(value []byte) InsertArg {
	return func(_ *Modification, d *TaskData) {
		d.Value = value
	}
}

// WithID sets the task's ID for insertion. This is not normally needed, as the backend
// will assign a new, unique ID for this task if none is specified. There are cases
// where assigning an explicit insertion ID (always being careful that it is
// unique) can be useful, however.
//
// For example, a not uncommon need is for a worker to do the following:
//
// 	- Claim a task,
// 	- Make database entries corresponding to downstream work,
// 	- Insert tasks for the downstream work and delete claimed task.
//
// If the database entries need to reference the tasks that have not yet been
// inserted (e.g., if they need to be used to get at the status of a task), it
// is not safe to simply update the database after insertion, as this introduces
// a race condition. If, for example, the following strategy is employed, then
// the task IDs may never make it into the database:
//
// 	- Claim a task,
// 	- Make database entries
// 	- Insert tasks and delete claimed task
// 	- Update database with new task IDs
//
// In this event, it is entirely possible to successfully process the incoming
// task and create the outgoing tasks, then lose network connectivity and fail
// to add those IDs to the databse. Now it is no longer possible to update the
// database appropriately: the task information is simply lost.
//
// Instead, it is safe to do the following:
//
// 	- Claim a task
// 	- Make database entries, including with to-be-created task IDs
// 	- Insert tasks with those IDs and delete claimed task.
//
// This avoids the potential data loss condition entirely.
//
// There are other workarounds for this situation, like using a two-step
// creation process and taking advantage of the ability to move tasks between
// queues without disturbing their ID (only their version), but this is not
// uncommon enough to warrant requiring the extra worker logic just to get a
// task ID into the database.
func WithID(id uuid.UUID) InsertArg {
	return func(_ *Modification, d *TaskData) {
		d.ID = id
	}
}

// WithSkipColliding sets the insert argument to allow itself to be removed if
// the only error encountered is an ID collision. This can help when it is
// desired to insert multiple tasks, but a previous subset was already inserted
// with similar IDs. Sometimes you want to specify a superset to "catch what we
// missed".
func WithSkipColliding(s bool) InsertArg {
	return func(_ *Modification, d *TaskData) {
		d.skipCollidingID = s
	}
}

// Deleting adds a deletion to a Modify call, e.g.,
//
//   cli.Modify(ctx, Deleting(id, version))
func Deleting(id uuid.UUID, version int32) ModifyArg {
	return func(m *Modification) {
		m.Deletes = append(m.Deletes, &TaskID{
			ID:      id,
			Version: version,
		})
	}
}

// DependingOn adds a dependency to a Modify call, e.g., to insert a task into
// "my queue" with data "hey", but only succeeding if a task with anotherID and
// someVersion exists:
//
//   cli.Modify(ctx,
//     InsertingInto("my queue",
//       WithValue([]byte("hey"))),
//       DependingOn(anotherID, someVersion))
func DependingOn(id uuid.UUID, version int32) ModifyArg {
	return func(m *Modification) {
		m.Depends = append(m.Depends, &TaskID{
			ID:      id,
			Version: version,
		})
	}
}

// Changing adds a task update to a Modify call, e.g., to modify
// the queue a task belongs in:
//
//   cli.Modify(ctx, Changing(myTask, QueueTo("a different queue name")))
func Changing(task *Task, changeArgs ...ChangeArg) ModifyArg {
	return func(m *Modification) {
		newTask := *task
		for _, a := range changeArgs {
			a(m, &newTask)
		}
		m.Changes = append(m.Changes, &newTask)
	}
}

// ChangeArg is an argument to the Changing function used to create arguments
// for Modify, e.g., to change the queue and set the expiry time of a task to
// 5 minutes in the future, you would do something like this:
//
//   cli.Modify(ctx,
//     Changing(myTask,
//       QueueTo("a new queue"),
//	     ArrivalTimeBy(5 * time.Minute)))
type ChangeArg func(m *Modification, t *Task)

// QueueTo creates an option to modify a task's queue in the Changing function.
func QueueTo(q string) ChangeArg {
	return func(_ *Modification, t *Task) {
		t.Queue = q
	}
}

// ArrivalTimeTo sets a specific arrival time on a changed task in the Changing function.
func ArrivalTimeTo(at time.Time) ChangeArg {
	return func(_ *Modification, t *Task) {
		t.At = at
	}
}

// ArrivalTimeBy sets the arrival time to a time in the future, by the given duration.
func ArrivalTimeBy(d time.Duration) ChangeArg {
	return func(m *Modification, t *Task) {
		t.At = m.now.Add(d)
	}
}

// ValueTo sets the changing task's value to the given byte slice.
func ValueTo(v []byte) ChangeArg {
	return func(_ *Modification, t *Task) {
		t.Value = v
	}
}

// WithModification returns a ModifyArg that merges the given Modification with whatever it is so far.
// Ignores Claimant field, and simply appends to all others.
func WithModification(src *Modification) ModifyArg {
	return func(dest *Modification) {
		dest.Inserts = append(dest.Inserts, src.Inserts...)
		dest.Changes = append(dest.Changes, src.Changes...)
		dest.Deletes = append(dest.Deletes, src.Deletes...)
		dest.Depends = append(dest.Depends, src.Depends...)
	}
}

// Modification contains all of the information for a single batch modification in the task store.
type Modification struct {
	now time.Time

	Claimant uuid.UUID

	Inserts []*TaskData
	Changes []*Task
	Deletes []*TaskID
	Depends []*TaskID
}

// NewModification creates a new modification: insertions, deletions, changes,
// and dependencies all together. When creating this for the purpose of passing
// to WithModification, set the claimant to uuid.Nil (it is ignored in that case).
func NewModification(claimant uuid.UUID, modArgs ...ModifyArg) *Modification {
	m := &Modification{
		Claimant: claimant,
		now:      time.Now(),
	}
	for _, arg := range modArgs {
		arg(m)
	}
	return m
}

// modDependencies returns a dependency map for all modified dependencies
// (deletes and changes).
func (m *Modification) modDependencies() (map[uuid.UUID]int32, error) {
	deps := make(map[uuid.UUID]int32)
	for _, t := range m.Changes {
		if _, ok := deps[t.ID]; ok {
			return nil, errors.New("duplicates found in dependencies")
		}
		deps[t.ID] = t.Version
	}
	for _, t := range m.Deletes {
		if _, ok := deps[t.ID]; ok {
			return nil, errors.New("duplicates found in dependencies")
		}
		deps[t.ID] = t.Version
	}
	return deps, nil
}

// AllDependencies returns a dependency map from ID to version for tasks that
// must exist and match version numbers. It returns an error if there are
// duplicates. Changes, Deletions, Dependencies and Insertions with IDs must be
// disjoint sets.
//
// When using this to query backend storage for the presence of tasks, note
// that it is safe to ignore the version if you use the DependencyError method
// to determine whether a transaction can proceed. That checks versions
// appropriate for all different types of modification.
func (m *Modification) AllDependencies() (map[uuid.UUID]int32, error) {
	deps, err := m.modDependencies()
	if err != nil {
		return nil, errors.Wrap(err, "get dependencies")
	}
	for _, t := range m.Depends {
		if _, ok := deps[t.ID]; ok {
			return nil, errors.New("duplicates found in dependencies")
		}
		deps[t.ID] = t.Version
	}
	for _, t := range m.Inserts {
		// Add to dependencies (they need to *not* exist) if an ID is set.
		if t.ID == uuid.Nil {
			continue
		}
		if _, ok := deps[t.ID]; ok {
			return nil, errors.New("duplicates found in dependencies")
		}
		deps[t.ID] = -1
	}
	return deps, nil
}

func (m *Modification) otherwiseClaimed(task *Task) bool {
	var zeroUUID uuid.UUID
	return task.At.After(m.now) && task.Claimant != zeroUUID && task.Claimant != m.Claimant
}

func (m *Modification) badInserts(foundDeps map[uuid.UUID]*Task) []*TaskID {
	var present []*TaskID
	for _, t := range m.Inserts {
		if found := foundDeps[t.ID]; found != nil {
			present = append(present, found.IDVersion())
		}
	}
	return present
}

func (m *Modification) badChanges(foundDeps map[uuid.UUID]*Task) (missing []*TaskID, claimed []*TaskID) {
	for _, t := range m.Changes {
		found := foundDeps[t.ID]
		if found == nil || found.Version != t.Version {
			missing = append(missing, &TaskID{
				ID:      t.ID,
				Version: t.Version,
			})
			continue
		}
		if m.otherwiseClaimed(found) {
			claimed = append(claimed, &TaskID{
				ID:      t.ID,
				Version: t.Version,
			})
			continue
		}
	}
	return missing, claimed
}

func (m *Modification) badDeletes(foundDeps map[uuid.UUID]*Task) (missing []*TaskID, claimed []*TaskID) {
	for _, t := range m.Deletes {
		found := foundDeps[t.ID]
		if found == nil || found.Version != t.Version {
			missing = append(missing, t)
			continue
		}
		if m.otherwiseClaimed(found) {
			claimed = append(claimed, t)
			continue
		}
	}
	return missing, claimed
}

func (m *Modification) missingDepends(foundDeps map[uuid.UUID]*Task) []*TaskID {
	var missing []*TaskID
	for _, t := range m.Depends {
		if found, ok := foundDeps[t.ID]; !ok || found.Version != t.Version {
			missing = append(missing, t)
		}
	}
	return missing
}

// DependencyError returns a DependencyError if there are problems with the
// dependencies found in the backend, or nil if everything is fine. Problems
// include missing or claimed dependencies, both of which will block a
// modification.
func (m *Modification) DependencyError(found map[uuid.UUID]*Task) error {
	presentInserts := m.badInserts(found)
	missingChanges, claimedChanges := m.badChanges(found)
	missingDeletes, claimedDeletes := m.badDeletes(found)
	missingDepends := m.missingDepends(found)

	if len(presentInserts) > 0 || len(missingChanges) > 0 || len(claimedChanges) > 0 || len(missingDeletes) > 0 || len(claimedDeletes) > 0 || len(missingDepends) > 0 {
		return DependencyError{
			Inserts: presentInserts,
			Changes: missingChanges,
			Deletes: missingDeletes,
			Depends: m.missingDepends(found),
			Claims:  append(claimedDeletes, claimedChanges...),
		}
	}
	return nil
}

// DependencyError is returned when a dependency is missing when modifying the task store.
type DependencyError struct {
	Inserts []*TaskID
	Depends []*TaskID
	Deletes []*TaskID
	Changes []*TaskID

	Claims []*TaskID

	Message string
}

// DependencyErrorf creates a new dependency error with the given message.
func DependencyErrorf(msg string, vals ...interface{}) DependencyError {
	return DependencyError{Message: fmt.Sprintf(msg, vals...)}
}

// Copy produces a new deep copy of this error type.
func (m DependencyError) Copy() DependencyError {
	e := DependencyError{
		Inserts: make([]*TaskID, len(m.Inserts)),
		Depends: make([]*TaskID, len(m.Depends)),
		Deletes: make([]*TaskID, len(m.Deletes)),
		Changes: make([]*TaskID, len(m.Changes)),
		Claims:  make([]*TaskID, len(m.Claims)),
		Message: m.Message,
	}
	copy(e.Inserts, m.Inserts)
	copy(e.Depends, m.Depends)
	copy(e.Deletes, m.Deletes)
	copy(e.Changes, m.Changes)
	copy(e.Claims, m.Claims)
	return e
}

// HasMissing indicates whether there was anything missing in this error.
func (m DependencyError) HasMissing() bool {
	return len(m.Depends) > 0 || len(m.Deletes) > 0 || len(m.Changes) > 0
}

// HasClaims indicates whether any of the tasks were claimed by another claimant and unexpired.
func (m DependencyError) HasClaims() bool {
	return len(m.Claims) > 0
}

// HasCollisions indicates whether any of the inserted tasks collided with existing IDs.
func (m DependencyError) HasCollisions() bool {
	return len(m.Inserts) > 0
}

// Error produces a helpful error string indicating what was missing.
func (m DependencyError) Error() string {
	lines := []string{
		fmt.Sprintf("DependencyError: %v", m.Message),
	}
	if len(m.Inserts) > 0 {
		lines = append(lines, "\tcolliding inserts:")
		for _, tid := range m.Inserts {
			lines = append(lines, fmt.Sprintf("\t\t%s", tid))
		}
	}
	if len(m.Depends) > 0 {
		lines = append(lines, "\tmissing depends:")
		for _, tid := range m.Depends {
			lines = append(lines, fmt.Sprintf("\t\t%s", tid))
		}
	}
	if len(m.Deletes) > 0 {
		lines = append(lines, "\tmissing deletes:")
		for _, tid := range m.Deletes {
			lines = append(lines, fmt.Sprintf("\t\t%s", tid))
		}
	}
	if len(m.Changes) > 0 {
		lines = append(lines, "\tmissing changes:")
		for _, tid := range m.Changes {
			lines = append(lines, fmt.Sprintf("\t\t%s", tid))
		}
	}
	if len(m.Claims) > 0 {
		lines = append(lines, "\tclaimed modified tasks:")
		for _, tid := range m.Claims {
			lines = append(lines, fmt.Sprintf("\t\t%s", tid))
		}
	}
	return strings.Join(lines, "\n")
}

// AsDependency indicates whether the given error is a dependency error.
func AsDependency(err error) (DependencyError, bool) {
	// Force two-lvalue context.
	d, ok := errors.Cause(err).(DependencyError)
	return d, ok
}

// Time gets the time as the backend understands it, in UTC. Default is just
// time.Now().UTC().
func (c *EntroQ) Time(ctx context.Context) (time.Time, error) {
	return ProcessTime(), nil
}

// ProcessTime returns the time the calling process thinks it is, in UTC.
func ProcessTime() time.Time {
	return time.Now().UTC()
}
