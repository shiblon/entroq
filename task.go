package entroq

import (
	"fmt"
	"strings"
	"time"
)

// TaskID contains the identifying parts of a task. If IDs don't match
// (identifier and version together), then operations fail on those tasks.
//
// Also contains the name of the queue in which this task resides. Can be
// omitted, as it does not effect functionality, but might be required for
// authorization, which is performed based on queue name. Present whenever
// using tasks as a source of IDs.
type TaskID struct {
	ID      string `json:"id"`
	Version int32  `json:"version"`

	Queue string `json:"queue,omitempty"`
}

// NewTaskID creates a new TaskID with given options.
func NewTaskID(id string, version int32, opts ...IDOption) *TaskID {
	tID := &TaskID{
		ID:      id,
		Version: version,
	}
	for _, o := range opts {
		o(tID)
	}
	return tID
}

// IDOption is an option for things that require task ID information. Allows additional ID-related metadata to be passed.
type IDOption func(id *TaskID)

// WithIDQueue specifies the queue for a particular task ID.
func WithIDQueue(q string) IDOption {
	return func(id *TaskID) {
		id.Queue = q
	}
}

// String produces the id:version string representation.
func (t TaskID) String() string {
	return fmt.Sprintf("%s:v%d (in %q)", t.ID, t.Version, t.Queue)
}

// Delete produces an appropriate ModifyArg to delete the task with this ID.
func (t TaskID) Delete() ModifyArg {
	return Deleting(t.ID, t.Version, WithIDQueue(t.Queue))
}

// Depend produces an appropriate ModifyArg to depend on this task ID.
func (t TaskID) Depend() ModifyArg {
	return DependingOn(t.ID, t.Version, WithIDQueue(t.Queue))
}

// TaskData contains just the data, not the identifier or metadata. Used for insertions.
type TaskData struct {
	Queue string    `json:"queue"`
	At    time.Time `json:"at"`
	Value []byte    `json:"value"`

	// Attempt indicates which "attempt number" this task is on. Used by workers.
	Attempt int32 `json:"attempt"`

	// Err contains error information for this task. Used by workers.
	Err string `json:"err"`

	// ID is an optional task ID to be used for task insertion.
	// Default empty causes one to be assigned, and that is
	// sufficient for many cases. If you desire to make a database entry that
	// *references* a task, however, in that case it can make sense to specify
	// an explicit task ID for insertion. This allows a common workflow cycle
	//
	// 	consume task -> db update -> insert tasks
	//
	// to be done safely, where the database update needs to refer to
	// to-be-inserted tasks.
	ID string `json:"id"`

	// skipCollidingID indicates that a collision on insertion is not fatal,
	// and the insertion can be removed if that happens, and then the
	// modification can be retried.
	skipCollidingID bool

	// These timings are here so that journaling can restore full state.
	// Usually they are blank, and there are no convenience methods to allow
	// them to be set. Leave them at default values in all cases.
	Created  time.Time `json:"created"`
	Modified time.Time `json:"modified"`
}

// String returns a string representation of the task data, excluding the value.
func (t *TaskData) String() string {
	s := fmt.Sprintf("%q::%v", t.Queue, t.At)
	if t.ID != "" {
		s += "::" + t.ID
	}
	return s
}

// Insert returns a ModifyArg that can be used in the Modify function to insert this task data.
func (t *TaskData) Insert() ModifyArg {
	return Inserting(t)
}

// Task represents a unit of work, with a byte slice value payload.
// Note that Claims is the number of times a task has successfully been claimed.
// This is different than the version number, which increments for
// every modification, not just claims.
type Task struct {
	Queue string `json:"queue"`

	ID      string `json:"id"`
	Version int32  `json:"version"`

	At       time.Time `json:"at"`
	Claimant string    `json:"claimant"`
	Claims   int32     `json:"claims"`
	Value    []byte    `json:"value"`

	Created  time.Time `json:"created"`
	Modified time.Time `json:"modified"`

	// FromQueue specifies the previous queue for a task that is moving to another queue.
	// Usually not present, can be used for change authorization (since two queues are in play, there).
	FromQueue string `json:"fromqueue,omitempty"`

	// Worker retry logic uses these fields when moving tasks and when retrying them.
	// It is left up to the consumer to determine how many attempts is too many
	// and to produce a suitable retry or move error.
	Attempt int32  `json:"attempt"`
	Err     string `json:"err"`
}

// String returns a useful representation of this task.
func (t *Task) String() string {
	qInfo := fmt.Sprintf("%q", t.Queue)
	if t.FromQueue != "" && t.FromQueue != t.Queue {
		qInfo = fmt.Sprintf("%q <- %q", t.Queue, t.FromQueue)
	}
	return fmt.Sprintf("Task [%s %s:v%d]\n\t", qInfo, t.ID, t.Version) + strings.Join([]string{
		fmt.Sprintf("at=%q claimant=%s claims=%d attempt=%d err=%q", t.At, t.Claimant, t.Claims, t.Attempt, t.Err),
		fmt.Sprintf("val=%q", string(t.Value)),
	}, "\n\t")
}

// Delete returns a ModifyArg that can be used in the Modify function, e.g.,
//
//	cli.Modify(ctx, task1.Delete())
//
// The above would cause the given task to be deleted, if it can be. It is
// shorthand for
//
//	cli.Modify(ctx, Deleting(task1.ID, task1.Version, WithIDQueue(task1.Queue)))
func (t *Task) Delete() ModifyArg {
	return Deleting(t.ID, t.Version, WithIDQueue(t.Queue))
}

// Change returns a ModifyArg that can be used in the Modify function, e.g.,
//
//	cli.Modify(ctx, task1.Change(ArrivalTimeBy(2 * time.Minute)))
//
// The above is shorthand for
//
//	cli.Modify(ctx, Changing(task1, ArrivalTimeBy(2 * time.Minute)))
func (t *Task) Change(args ...ChangeArg) ModifyArg {
	return Changing(t, args...)
}

// Depend returns a ModifyArg that can be used to create a Modify dependency, e.g.,
//
//	cli.Modify(ctx, task.Depend())
//
// That is shorthand for
//
//	cli.Modify(ctx, DependingOn(task.ID, task.Version, WithIDQueue(task.Queue)))
func (t *Task) Depend() ModifyArg {
	return DependingOn(t.ID, t.Version, WithIDQueue(t.Queue))
}

// RetryOrQuarantine returns a ModifyArg for cases where a task has an error that seems retriable.
// It increments attempts, sets the latest error message on the task, and
// compares against a maximum number of attempts to determine whether to move it
// to a "quarantine" queue so that it can be analyzed later.
// If afterMaxAttempts is 0, both it and the quarantineTo queue are ignored and
// this will only retry.
func (t *Task) RetryOrQuarantine(errMsg, quarantineTo string, afterMaxAttempts int32, overrides ...ChangeArg) ModifyArg {
	args := []ChangeArg{AttemptToNext(), AppendingErr(errMsg)}
	if quarantineTo != "" && afterMaxAttempts != 0 && t.Attempt+1 >= afterMaxAttempts {
		args = append(args, QueueTo(quarantineTo))
	}
	args = append(args, overrides...)
	return Changing(t, args...)
}

// Retry adds an error and increments attempts while adding time to At.
func (t *Task) Retry(errMsg string, overrides ...ChangeArg) ModifyArg {
	return t.RetryOrQuarantine(errMsg, "", 0, overrides...)
}

// Quarantine adds an error and shuffles this off to a quarantine queue.
// Quarantine queues are just queues. They aren't special. What makes this a
// quarantine is the fact that Attempt is incremented and an error message is
// present.
func (t *Task) Quarantine(errMsg, toQ string, overrides ...ChangeArg) ModifyArg {
	return t.RetryOrQuarantine(errMsg, toQ, 1, overrides...)
}

// ID returns a Task ID from this task.
func (t *Task) IDVersion() *TaskID {
	return NewTaskID(t.ID, t.Version, WithIDQueue(t.Queue))
}

// Data returns the data for this task.
func (t *Task) Data() *TaskData {
	return &TaskData{
		Queue:    t.Queue,
		At:       t.At,
		Value:    t.Value,
		ID:       t.ID,
		Attempt:  t.Attempt,
		Err:      t.Err,
		Created:  t.Created,
		Modified: t.Modified,
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

// CopyOmitValue copies this task but leaves the value blank.
func (t *Task) CopyOmitValue() *Task {
	newT := new(Task)
	*newT = *t
	newT.Value = nil
	return newT
}

// CopyWithValue lets you specify whether the value should be copied.
func (t *Task) CopyWithValue(ok bool) *Task {
	if ok {
		return t.Copy()
	}
	return t.CopyOmitValue()
}
