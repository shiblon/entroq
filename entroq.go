// Package entroq contains the main task queue client and data definitions. The
// client relies on a backend to implement the actual transactional
// functionality, the interface for which is also defined here.
package entroq

import (
	"context"
	"fmt"
	"strings"
	"time"

	uuid "github.com/google/uuid"
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
	Queue string

	ID      uuid.UUID
	Version int32

	At       time.Time
	Claimant uuid.UUID
	Value    []byte

	Created  time.Time
	Modified time.Time
}

// String returns a string representation of this task.
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

// Backend describes all of the functions that any backend has to implement
// to be used as the storage for task queues.
type Backend interface {
	// Queues returns all known non-empty queue names.
	Queues(ctx context.Context) ([]string, error)

	// Tasks retrieves all tasks from the given queue. If claimantID is
	// specified (non-zero), limits those tasks to those that are either
	// expired or belong to the given claimant. Otherwise returns them all.
	Tasks(ctx context.Context, queue string, claimant uuid.UUID) ([]*Task, error)

	// TryClaim attempts to claim a task from the "top" (or close to it) of the
	// given queue. When claimed, a task is held for the duration specified
	// from the time of the claim. If claiming until a specific wall-clock time
	// is desired, the task should be immediately modified after it is claimed
	// to set the AT to a specific time. Returns a nil task and a nil error if
	// there is nothing to claim.
	TryClaim(ctx context.Context, queue string, claimant uuid.UUID, duration time.Duration) (*Task, error)

	// Modify attempts to atomically modify the task store, and only succeeds
	// if all dependencies are available and all mutations are either expired
	// or already owned by this claimant. The Modification type provides useful
	// functions for determining whether dependencies are good or bad. This
	// function is intended to return a DependencyError if the transaction could
	// not proceed because dependencies were missing or already claimed (and
	// not expired) by another claimant.
	Modify(ctx context.Context, claimant uuid.UUID, mod *Modification) (inserted []*Task, changed []*Task, err error)

	// Close cleans up the backend connection.
	Close() error
}

// Open is a function that produces a Backend from a context.
// Backend implementations will typically provide one of these
type Open func(ctx context.Context) (Backend, error)

// Client is a client interface for accessing tasks implemented in PostgreSQL.
type Client struct {
	backend  Backend
	claimant uuid.UUID
}

// NewClient creates a new task client with the given backend implementation.
//
//   cli := NewClient(backend, WithClaimant(myClaimantID))
func NewClient(ctx context.Context, open Open, opts ...ClientOption) (*Client, error) {
	backend, err := open(ctx)
	if err != nil {
		return nil, err
	}
	client := &Client{
		claimant: uuid.New(),
		backend:  backend,
	}
	for _, opt := range opts {
		opt(client)
	}
	return client, nil
}

// ClientOption is used to pass options to NewClient.
type ClientOption func(c *Client)

// WithClaimant sets the claimant ID for this client, e.g.,
//   NewClient(backend, WithClaimant(uuid.New()))
func WithClaimant(claimant uuid.UUID) ClientOption {
	return func(c *Client) {
		c.claimant = claimant
	}
}

// Close cleans up and closes the underlying connection.
func (c *Client) Close() error {
	return c.backend.Close()
}

// Queues returns a slice of all queue names.
func (c *Client) Queues(ctx context.Context) ([]string, error) {
	return c.backend.Queues(ctx)
}

// Tasks returns a slice of all tasks in the given queue.
func (c *Client) Tasks(ctx context.Context, queue string, opts ...TasksOpt) ([]*Task, error) {
	optVals := new(tasksOpts)
	for _, opt := range opts {
		opt(optVals)
	}
	var claimant uuid.UUID
	if !optVals.allowClaimed {
		claimant = c.claimant
	}
	return c.backend.Tasks(ctx, queue, claimant)
}

type tasksOpts struct {
	allowClaimed bool
}

// TasksOpt is an option that can be passed into Tasks to control what it returns.
type TasksOpt func(*tasksOpts)

// IncludeClaimed indicates that future tasks claimed by another can be listed.
func IncludeClaimed() TasksOpt {
	return func(a *tasksOpts) {
		a.allowClaimed = true
	}
}

// Claim attempts to get the next unclaimed task from the given queue. It
// blocks until one becomes available or until the context is done. When it
// succeeds, it returns a task with the claimant set to this client, and an
// arrival time given by the duration.
func (c *Client) Claim(ctx context.Context, q string, duration time.Duration) (*Task, error) {
	const maxWait = time.Minute
	var curWait = time.Second
	for {
		task, err := c.TryClaim(ctx, q, duration)
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
			return nil, fmt.Errorf("context canceled for claim request in %q", q)
		}
	}
}

// TryClaimTask attempts one time to claim a task from the given queue. If there are no tasks, it
// returns a nil error *and* a nil task. This allows the caller to decide whether to retry.
func (c *Client) TryClaim(ctx context.Context, q string, duration time.Duration) (*Task, error) {
	return c.backend.TryClaim(ctx, q, c.claimant, duration)
}

// Modify allows a batch modification operation to be done, gated on the
// existence of all task IDs and versions specified. Deletions, Updates, and
// Dependencies must be present. The transaction all fails or all succeeds.
//
// Returns all inserted task IDs, and an error if it could not proceed. If the error
// was due to missing dependencies, a *DependencyError is returned, which can be checked for
// by calling IsDependency(err).
func (c *Client) Modify(ctx context.Context, modArgs ...ModifyArg) (inserted []*Task, changed []*Task, err error) {
	mod := newModification(c.claimant)
	for _, arg := range modArgs {
		arg(mod)
	}

	return c.backend.Modify(ctx, c.claimant, mod)
}

// ModifyArg is an argument to the Modify function, which does batch modifications to the task store.
type ModifyArg func(m *Modification)

// InsertingInto creates an insert modification. Use like this:
//
//   cli.Modify(InsertingInto("my queue name", WithValue([]byte("hi there"))))
func InsertingInto(q string, insertArgs ...InsertArg) ModifyArg {
	return func(m *Modification) {
		data := &TaskData{Queue: q}
		for _, arg := range insertArgs {
			arg(m.now, data)
		}
		m.Inserts = append(m.Inserts, data)
	}
}

// InsertArg is an argument to task insertion.
type InsertArg func(now time.Time, d *TaskData)

// WithArrivalTime changes the arrival time to a fixed moment during task insertion.
func WithArrivalTime(at time.Time) InsertArg {
	return func(now time.Time, d *TaskData) {
		d.At = at
	}
}

// WithArrivalTimeIn computes the arrival time based on the duration from now, e.g.,
//
//   cli.Modify(ctx,
//     InsertingInto("my queue",
//       WithTimeIn(2 * time.Minute)))
func WithArrivalTimeIn(duration time.Duration) InsertArg {
	return func(now time.Time, d *TaskData) {
		d.At = now.Add(duration)
	}
}

// WithValue sets the task's byte slice value during insertion.
//   cli.Modify(ctx,
//     InsertingInto("my queue",
//       WithValue([]byte("hi there"))))
func WithValue(value []byte) InsertArg {
	return func(now time.Time, d *TaskData) {
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
			a(&newTask)
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
type ChangeArg func(t *Task)

// QueueTo creates an option to modify a task's queue in the Changing function.
func QueueTo(q string) ChangeArg {
	return func(t *Task) {
		t.Queue = q
	}
}

// ArrivalTimeTo sets a specific arrival time on a changed task in the Changing function.
func ArrivalTimeTo(at time.Time) ChangeArg {
	return func(t *Task) {
		t.At = at
	}
}

// ArrivalTimeBy sets the arrival time to a time in the future, by the given duration.
func ArrivalTimeBy(d time.Duration) ChangeArg {
	return func(t *Task) {
		t.At = t.At.Add(d)
	}
}

// ValueTo sets the changing task's value to the given byte slice.
func ValueTo(v []byte) ChangeArg {
	return func(t *Task) {
		t.Value = v
	}
}

// modification contains all of the information for a single batch modification in the task store.
type Modification struct {
	claimant uuid.UUID
	now      time.Time

	Inserts []*TaskData
	Changes []*Task
	Deletes []*TaskID
	Depends []*TaskID
}

func newModification(claimant uuid.UUID) *Modification {
	return &Modification{claimant: claimant, now: time.Now()}
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
	return task.At.After(m.now) && task.Claimant != zeroUUID && task.Claimant != m.claimant
}

func (m *Modification) badChanges(foundDeps map[uuid.UUID]*Task) (missing []*TaskID, claimed []*TaskID) {
	for _, t := range m.Changes {
		found := foundDeps[t.ID]
		if found == nil {
			missing = append(missing, &TaskID{
				ID:      t.ID,
				Version: t.Version,
			})
		}
		if m.otherwiseClaimed(found) {
			claimed = append(claimed, &TaskID{
				ID:      t.ID,
				Version: t.Version,
			})
		}
	}
	return missing, claimed
}

func (m *Modification) badDeletes(foundDeps map[uuid.UUID]*Task) (missing []*TaskID, claimed []*TaskID) {
	for _, t := range m.Deletes {
		found := foundDeps[t.ID]
		if found == nil {
			missing = append(missing, t)
		}
		if m.otherwiseClaimed(found) {
			claimed = append(claimed, t)
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
