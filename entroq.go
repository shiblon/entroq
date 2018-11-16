// Package entroq contains the main task queue client and data definitions. The
// client relies on a backend to implement the actual transactional
// functionality, the interface for which is also defined here.
package entroq

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

// TaskID contains the identifying parts of a task. If IDs don't match
// (identifier and version together), then operations fail on those tasks.
type TaskID struct {
	ID      uuid.UUID
	Version int32
}

func (t TaskID) String() string {
	return fmt.Sprintf("%s:v%d", t.ID, t.Version)
}

// TaskData contains just the data, not the identifier or metadata. Used for insertions.
type TaskData struct {
	Queue string
	At    time.Time
	Value []byte
}

// Task represents a unit of work, with a byte slice value payload.
type Task struct {
	Queue string `json:"queue"`

	ID      uuid.UUID `json:"id"`
	Version int32     `json:"version"`

	At       time.Time `json:"at"`
	Claimant uuid.UUID `json:"claimant"`
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
	}
}

// Copy copies this tasks data and everything.
func (t *Task) Copy() *Task {
	newT := new(Task)
	*newT = *t
	newT.Value = make([]byte, len(t.Value))
	copy(newT.Value, t.Value)
	return newT
}

// ClaimQuery contains information necessary to attempt to make a claim on a task in a specific queue.
type ClaimQuery struct {
	Queue    string        // The name of the queue to claim from.
	Claimant uuid.UUID     // The ID of the process trying to make the claim.
	Duration time.Duration // How long the task should be claimed for if successful.
}

// TasksQuery holds information for a tasks query.
type TasksQuery struct {
	Queue    string
	Claimant uuid.UUID
	Limit    int
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

// Backend describes all of the functions that any backend has to implement
// to be used as the storage for task queues.
type Backend interface {
	// Queues returns a mapping from all known queues to their task counts.
	Queues(ctx context.Context, qq *QueuesQuery) (map[string]int, error)

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

	// Modify attempts to atomically modify the task store, and only succeeds
	// if all dependencies are available and all mutations are either expired
	// or already owned by this claimant. The Modification type provides useful
	// functions for determining whether dependencies are good or bad. This
	// function is intended to return a DependencyError if the transaction could
	// not proceed because dependencies were missing or already claimed (and
	// not expired) by another claimant.
	Modify(ctx context.Context, mod *Modification) (inserted []*Task, changed []*Task, err error)

	// Close closes any underlying connections. The backend is expected to take
	// ownership of all such connections, so this cleans them up.
	Close() error
}

// EntroQ is a client interface for accessing the task queue.
type EntroQ struct {
	backend Backend
}

// BackendOpener is a function that can open a connection to a backend. Creating
// a client with a specific backend is accomplished by passing one of these functions
// into New.
type BackendOpener func(ctx context.Context) (Backend, error)

// New creates a new task client with the given backend implementation.
//
//   cli := New(ctx, backend)
func New(ctx context.Context, opener BackendOpener) (*EntroQ, error) {
	backend, err := opener(ctx)
	if err != nil {
		return nil, fmt.Errorf("backend open failed: %v", err)
	}
	return &EntroQ{backend: backend}, nil
}

// Close closes the underlying backend.
func (c *EntroQ) Close() error {
	return c.backend.Close()
}

// Queues returns a mapping from all queue names to their task counts.
func (c *EntroQ) Queues(ctx context.Context, opts ...QueuesOpt) (map[string]int, error) {
	query := new(QueuesQuery)
	for _, opt := range opts {
		opt(query)
	}
	return c.backend.Queues(ctx, query)
}

// QueueEmpty indicates whether the specified task queues are all empty. If no
// options are specified, returns an error.
func (c *EntroQ) QueuesEmpty(ctx context.Context, opts ...QueuesOpt) (bool, error) {
	if len(opts) == 0 {
		return false, fmt.Errorf("empty check: no queue options specified")
	}
	qs, err := c.Queues(ctx, opts...)
	if err != nil {
		return false, fmt.Errorf("empty check: %v", err)
	}
	for _, size := range qs {
		if size > 0 {
			return false, nil
		}
	}
	return true, nil
}

// Tasks returns a slice of all tasks in the given queue.
func (c *EntroQ) Tasks(ctx context.Context, queue string, opts ...TasksOpt) ([]*Task, error) {
	query := &TasksQuery{
		Queue: queue,
	}
	for _, opt := range opts {
		opt(query)
	}

	return c.backend.Tasks(ctx, query)
}

// TasksOpt is an option that can be passed into Tasks to control what it returns.
type TasksOpt func(*TasksQuery)

// LimitClaimant sets the claimant and then only returns self-claimed tasks or expired tasks.
func LimitClaimant(claimant uuid.UUID) TasksOpt {
	return func(q *TasksQuery) {
		q.Claimant = claimant
	}
}

// LimitTasks sets the limit on the number of tasks to return. A value <= 0 indicates "no limit".
func LimitTasks(limit int) TasksOpt {
	return func(q *TasksQuery) {
		q.Limit = limit
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

// Canceled is an error returned when a claim is canceled by its context.
type Canceled error

// IsCanceled indicates whether the error is a canceled error.
func IsCanceled(err error) bool {
	_, ok := err.(Canceled)
	return ok
}

// Claim attempts to get the next unclaimed task from the given queue. It
// blocks until one becomes available or until the context is done. When it
// succeeds, it returns a task with the claimant set to that given, and an
// arrival time given by the duration.
func (c *EntroQ) Claim(ctx context.Context, claimant uuid.UUID, q string, duration time.Duration, opts ...ClaimOpt) (*Task, error) {
	const maxWait = time.Minute
	var curWait = time.Second
	for {
		task, err := c.TryClaim(ctx, claimant, q, duration, opts...)
		if err != nil {
			return nil, err
		}
		if task != nil {
			return task, nil
		}
		// No error, no task - we wait.
		select {
		case <-time.After(curWait):
			curWait *= 2
			if curWait > maxWait {
				curWait = maxWait
			}
		case <-ctx.Done():
			return nil, Canceled(fmt.Errorf("context canceled for claim request in %q", q))
		}
	}
}

// TryClaimTask attempts one time to claim a task from the given queue. If there are no tasks, it
// returns a nil error *and* a nil task. This allows the caller to decide whether to retry. It can fail if certain (optional) dependency tasks are not present. This can be used, for example, to ensure that configuration tasks haven't changed.
func (c *EntroQ) TryClaim(ctx context.Context, claimant uuid.UUID, q string, duration time.Duration, opts ...ClaimOpt) (*Task, error) {
	query := &ClaimQuery{
		Queue:    q,
		Claimant: claimant,
		Duration: duration,
	}
	for _, opt := range opts {
		opt(query)
	}
	return c.backend.TryClaim(ctx, query)
}

// RenewFor attempts to renew the given task's lease (update arrival time) for
// the given duration. Returns the new task.
func (c *EntroQ) RenewFor(ctx context.Context, task *Task, duration time.Duration) (*Task, error) {
	changed, err := c.RenewAllFor(ctx, []*Task{task}, duration)
	if err != nil {
		return nil, fmt.Errorf("renew task: %v", err)
	}
	return changed[0], nil
}

// RenewAllFor attempts to renew all given tasks' leases (update arrival times)
// for the given duration. Returns the new tasks.
func (c *EntroQ) RenewAllFor(ctx context.Context, tasks []*Task, duration time.Duration) ([]*Task, error) {
	if len(tasks) == 0 {
		return nil, nil
	}
	var modArgs []ModifyArg
	var taskIDs []string
	for _, t := range tasks {
		modArgs = append(modArgs, Changing(t, ArrivalTimeBy(duration)))
		taskIDs = append(taskIDs, t.IDVersion().String())
	}
	_, changed, err := c.Modify(ctx, tasks[0].Claimant, modArgs...)
	if err != nil {
		return nil, fmt.Errorf("renewal failed for tasks %q", taskIDs)
	}
	if len(changed) != len(tasks) {
		return nil, fmt.Errorf("renewal expected %d updated tasks, got %d", len(tasks), len(changed))
	}
	return changed, nil
}

// DoWithRenewAll runs the provided function while keeping all given tasks leases renewed.
func (c *EntroQ) DoWithRenewAll(ctx context.Context, tasks []*Task, d time.Duration, f func(context.Context, []uuid.UUID, []*TaskData) error) ([]*Task, error) {
	// Save the initial data and ID, so the renewal function can get at them without synchronization.
	var ids []uuid.UUID
	var dats []*TaskData
	for _, t := range tasks {
		ids = append(ids, t.ID)
		dats = append(dats, t.Data())
	}

	g, ctx := errgroup.WithContext(ctx)

	doneCh := make(chan struct{})

	g.Go(func() error {
		for {
			select {
			case <-doneCh:
				return nil
			case <-ctx.Done():
				return fmt.Errorf("canceled context, stopped renewing")
			case <-time.After(d / 2):
				var err error
				if tasks, err = c.RenewAllFor(ctx, tasks, d); err != nil {
					return fmt.Errorf("could not extend lease for tasks %v: %v", ids, err)
				}
			}
		}
	})

	g.Go(func() error {
		defer close(doneCh)
		return f(ctx, ids, dats)
	})

	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("error running with renewal of tasks %v: %v", ids, err)
	}

	// The task will have been overwritten with every renewal. Return final task.
	return tasks, nil
}

// DoWithRenew runs the provided function while keeping the given task lease renewed.
func (c *EntroQ) DoWithRenew(ctx context.Context, task *Task, d time.Duration, f func(context.Context, uuid.UUID, *TaskData) error) (*Task, error) {
	finalTasks, err := c.DoWithRenewAll(ctx, []*Task{task}, d, func(ctx context.Context, ids []uuid.UUID, dats []*TaskData) error {
		return f(ctx, ids[0], dats[0])
	})
	if err != nil {
		return nil, fmt.Errorf("with renew single task: %v", err)
	}
	return finalTasks[0], nil
}

// Modify allows a batch modification operation to be done, gated on the
// existence of all task IDs and versions specified. Deletions, Updates, and
// Dependencies must be present. The transaction all fails or all succeeds.
//
// Returns all inserted task IDs, and an error if it could not proceed. If the error
// was due to missing dependencies, a *DependencyError is returned, which can be checked for
// by calling IsDependency(err).
func (c *EntroQ) Modify(ctx context.Context, claimant uuid.UUID, modArgs ...ModifyArg) (inserted []*Task, changed []*Task, err error) {
	mod := NewModification(claimant)
	for _, arg := range modArgs {
		arg(mod)
	}

	return c.backend.Modify(ctx, mod)
}

// ModifyArg is an argument to the Modify function, which does batch modifications to the task store.
type ModifyArg func(m *Modification)

// Inserting creates an insert modification from TaskData:
//
// 	cli.Modify(Inserting(&TaskData{
// 		Queue: "myqueue",
// 		At:    time.Now.Add(1 * time.Minute),
// 		Value: []byte("hi there"),
// 	}))
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

// modification contains all of the information for a single batch modification in the task store.
type Modification struct {
	now time.Time

	Claimant uuid.UUID

	Inserts []*TaskData
	Changes []*Task
	Deletes []*TaskID
	Depends []*TaskID
}

func NewModification(claimant uuid.UUID) *Modification {
	return &Modification{
		Claimant: claimant,
		now:      time.Now(),
	}
}

// modDependencies returns a dependency map for all modified dependencies
// (deletes and changes).
func (m *Modification) modDependencies() (map[uuid.UUID]int32, error) {
	deps := make(map[uuid.UUID]int32)
	for _, t := range m.Changes {
		if _, ok := deps[t.ID]; ok {
			return nil, fmt.Errorf("duplicates found in dependencies")
		}
		deps[t.ID] = t.Version
	}
	for _, t := range m.Deletes {
		if _, ok := deps[t.ID]; ok {
			return nil, fmt.Errorf("duplicates found in dependencies")
		}
		deps[t.ID] = t.Version
	}
	return deps, nil
}

// AllDependencies returns a dependency map from ID to version, and returns an
// error if there are duplicates. Changes, Deletions, and Dependencies should
// be disjoint sets.
func (m *Modification) AllDependencies() (map[uuid.UUID]int32, error) {
	deps, err := m.modDependencies()
	if err != nil {
		return nil, err
	}
	for _, t := range m.Depends {
		if _, ok := deps[t.ID]; ok {
			return nil, fmt.Errorf("duplicates found in dependencies")
		}
		deps[t.ID] = t.Version
	}
	return deps, nil
}

func (m *Modification) otherwiseClaimed(task *Task) bool {
	var zeroUUID uuid.UUID
	return task.At.After(m.now) && task.Claimant != zeroUUID && task.Claimant != m.Claimant
}

func (m *Modification) badChanges(foundDeps map[uuid.UUID]*Task) (missing []*TaskID, claimed []*TaskID) {
	for _, t := range m.Changes {
		found := foundDeps[t.ID]
		if found == nil {
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
		if found == nil {
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
		if _, ok := foundDeps[t.ID]; !ok {
			missing = append(missing, t)
		}
	}
	return missing
}

// Returns a DependencyError if there are problems with the
// dependencies found in the backend, or nil if everything is
// fine. Problems include missing or claimed dependencies, both of
// which will block a modification.
func (m *Modification) DependencyError(found map[uuid.UUID]*Task) error {
	missingChanges, claimedChanges := m.badChanges(found)
	missingDeletes, claimedDeletes := m.badDeletes(found)
	missingDepends := m.missingDepends(found)

	if len(missingChanges) > 0 || len(claimedChanges) > 0 || len(missingDeletes) > 0 || len(claimedDeletes) > 0 || len(missingDepends) > 0 {
		return DependencyError{
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
	Depends []*TaskID
	Deletes []*TaskID
	Changes []*TaskID

	Claims []*TaskID
}

// HasMissing indicates whether there was anything missing in this error.
func (m DependencyError) HasMissing() bool {
	return len(m.Depends) > 0 || len(m.Deletes) > 0 || len(m.Changes) > 0
}

// HasClaims indicates whether any of the tasks were claimed by another claimant and unexpired.
func (m DependencyError) HasClaims() bool {
	return len(m.Claims) > 0
}

// Error produces a helpful error string indicating what was missing.
func (m DependencyError) Error() string {
	lines := []string{
		"DependencyError:",
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

// IsDependency indicates whether the given error is a dependency error.
func IsDependency(err error) bool {
	_, ok := err.(DependencyError)
	return ok
}
