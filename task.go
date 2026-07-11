package entroq

import (
	"encoding/json"
	"fmt"
	"log"
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
func NewTaskID(id string, version int32, queue string) *TaskID {
	return &TaskID{
		ID:      id,
		Version: version,
		Queue: queue,
	}
}

// String produces the id:version string representation.
func (t TaskID) String() string {
	return fmt.Sprintf("%s:v%d (in %q)", t.ID, t.Version, t.Queue)
}

// Delete produces an appropriate ModifyArg to delete the task with this ID.
func (t TaskID) Delete() ModifyArg {
	return func(m *Modification) {
		m.Deletes = append(m.Deletes, &t)
	}
}

// Depend produces an appropriate ModifyArg to depend on this task ID.
func (t TaskID) Depend() ModifyArg {
	return func(m *Modification) {
		m.Depends = append(m.Depends, &t)
	}
}

// TaskData contains just the data, not the identifier or metadata. Used for insertions.
type TaskData struct {
	Queue string          `json:"queue"`
	At    time.Time       `json:"at"`
	Value json.RawMessage `json:"value"`

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

// InsertArg is an argument to task insertion.
type InsertArg func(*Modification, *TaskData)

// WithArrivalTime changes the arrival time to a fixed moment during task insertion.
// The time is taken as-is from the caller. If tight synchronization with the
// backend clock is required, use EntroQ.Time to obtain the reference time first.
func WithArrivalTime(at time.Time) InsertArg {
	return func(_ *Modification, d *TaskData) {
		d.At = at
	}
}

// WithArrivalTimeIn computes the arrival time based on the duration from now, e.g.,
//
//	cli.Modify(ctx,
//	  InsertingInto("my queue",
//	    WithTimeIn(2 * time.Minute)))
//
// The duration is added to Go's wall clock time at the point of Modify. If tight
// synchronization with the backend clock is required, use EntroQ.Time and
// WithArrivalTime instead.
func WithArrivalTimeIn(duration time.Duration) InsertArg {
	return func(m *Modification, d *TaskData) {
		d.At = m.now.Add(duration)
	}
}

// WithRawValue sets the task's JSON value during insertion from pre-marshaled
// bytes. The value must be valid JSON; nil is allowed and represents an absent
// value. Use WithValue to marshal a Go value on the fly.
//
//	cli.Modify(ctx,
//	  InsertingInto("my queue",
//	    WithRawValue(json.RawMessage(`"hi there"`))))
func WithRawValue(value json.RawMessage) InsertArg {
	return func(_ *Modification, d *TaskData) {
		d.Value = value
	}
}

// WithValue marshals v as JSON and uses it as the task value. It is a
// "Must"-style function: if v cannot be marshaled (channels, functions,
// cycles), it calls log.Fatal. These are programmer errors, not runtime
// conditions -- the type being marshaled is known at compile time. Use
// WithRawValue for pre-marshaled data.
//
//	cli.Modify(ctx,
//	  InsertingInto("my queue",
//	    WithValue(MyStruct{Field: "hello"})))
func WithValue(v any) InsertArg {
	b, err := json.Marshal(v)
	if err != nil {
		log.Fatalf("entroq: WithValue: %v", err)
	}
	return WithRawValue(b)
}

// WithAttempt sets the number of attempts for this task. Usually not needed,
// handled automatically by the worker.
func WithAttempt(value int32) InsertArg {
	return func(_ *Modification, d *TaskData) {
		d.Attempt = value
	}
}

// WithErr sets the error field of a task during insertion. Usually not needed,
// as tasks are typically modified to add errors, not inserted with them.
func WithErr(value string) InsertArg {
	return func(_ *Modification, d *TaskData) {
		d.Err = value
	}
}

// WithID sets the task's ID for insertion. This is not normally needed, as the backend
// will assign a new, unique ID for this task if none is specified. There are cases
// where assigning an explicit insertion ID (always being careful that it is
// unique) can be useful, however.
//
// NOTE: IDs must be <= 64 characters in length for some backends.
//
// For example, a not uncommon need is for a worker to do the following:
//
//   - Claim a task,
//   - Make database entries corresponding to downstream work,
//   - Insert tasks for the downstream work and delete claimed task.
//
// If the database entries need to reference the tasks that have not yet been
// inserted (e.g., if they need to be used to get at the status of a task), it
// is not safe to simply update the database after insertion, as this introduces
// a race condition. If, for example, the following strategy is employed, then
// the task IDs may never make it into the database:
//
//   - Claim a task,
//   - Make database entries
//   - Insert tasks and delete claimed task
//   - Update database with new task IDs
//
// In this event, it is entirely possible to successfully process the incoming
// task and create the outgoing tasks, then lose network connectivity and fail
// to add those IDs to the databse. Now it is no longer possible to update the
// database appropriately: the task information is simply lost.
//
// Instead, it is safe to do the following:
//
//   - Claim a task
//   - Make database entries, including with to-be-created task IDs
//   - Insert tasks with those IDs and delete claimed task.
//
// This avoids the potential data loss condition entirely.
//
// There are other workarounds for this situation, like using a two-step
// creation process and taking advantage of the ability to move tasks between
// queues without disturbing their ID (only their version), but this is not
// uncommon enough to warrant requiring the extra worker logic just to get a
// task ID into the database.
func WithID(id string) InsertArg {
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

// Inserting creates an insert modification from TaskData:
//
//	cli.Modify(ctx,
//		Inserting(&TaskData{
//			Queue: "myqueue",
//			At:    time.Now.Add(1 * time.Minute),
//			Value: json.RawMessage(`"hi there"`),
//		}))
//
// Or, preferred:
//
//	cli.Modify(ctx,
//		InsertingInto("myqueue",
//		    WithArrivalTimeIn(1 * time.Minute),
//		    WithValue(json.RawMessage(`"hi there"`))))
func Inserting(tds ...*TaskData) ModifyArg {
	return func(m *Modification) {
		m.Inserts = append(m.Inserts, tds...)
	}
}

// InsertingInto creates an insert modification. Use like this:
//
//	cli.Modify(InsertingInto("my queue name", WithValue(json.RawMessage(`"hi there"`))))
func InsertingInto(q string, insertArgs ...InsertArg) ModifyArg {
	return func(m *Modification) {
		data := &TaskData{Queue: q}
		for _, arg := range insertArgs {
			arg(m, data)
		}
		m.Inserts = append(m.Inserts, data)
	}
}

// Task represents a unit of work, with a byte slice value payload.
// Note that Claims is the number of times a task has successfully been claimed.
// This is different than the version number, which increments for
// every modification, not just claims.
type Task struct {
	Queue string `json:"queue"`

	ID      string `json:"id"`
	Version int32  `json:"version"`

	At       time.Time       `json:"at"`
	Claimant string          `json:"claimant"`
	Claims   int32           `json:"claims"`
	Value    json.RawMessage `json:"value"`

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
func (t *Task) Delete() ModifyArg {
	return t.IDVersion().Delete()
}

// ChangeArg is an argument to the Task.Change function used to create arguments
// for Modify, e.g., to change the queue and set the expiry time of a task to
// 5 minutes in the future, you would do something like this:
//
//	  cli.Modify(ctx,
//	    myTask.Change(
//	      QueueTo("a new queue"),
//		  ArrivalTimeBy(5 * time.Minute)))
type ChangeArg func(m *Modification, t *Task)

// QueueTo creates an option to modify a task's queue in Task.Change.
func QueueTo(q string) ChangeArg {
	return func(_ *Modification, t *Task) {
		// Save the old queue for authorization to move this from one to another.
		t.FromQueue = t.Queue
		t.Queue = q
	}
}

// ArrivalTimeTo sets a specific arrival time on a changed task in Task.Change.
func ArrivalTimeTo(at time.Time) ChangeArg {
	return func(_ *Modification, t *Task) {
		t.At = at
	}
}

// ArrivalTimeBy sets the arrival time to a time in the future, by the given duration.
// The duration is added to Go's wall clock time at the point of Modify. If tight
// synchronization with the backend clock is required, use EntroQ.Time and
// WithArrivalTime instead. Send a duration of 0 for immediate availability.
func ArrivalTimeBy(d time.Duration) ChangeArg {
	return func(m *Modification, t *Task) {
		t.At = m.now.Add(d)
	}
}

// RawValueTo sets the changing task's JSON value from pre-marshaled bytes.
// The value must be valid JSON; nil is allowed and represents an absent value.
func RawValueTo(v json.RawMessage) ChangeArg {
	return func(_ *Modification, t *Task) {
		t.Value = v
	}
}

// ValueTo sets the changing task's JSON value by marshaling what is passed in
// first.
func ValueTo(v any) ChangeArg {
	// Errors are code errors if something unmarshalable is passed (like chan).
	b, err := json.Marshal(v)
	if err != nil {
		log.Fatalf("unmarshalable type ValueTo: %v", err)
	}
	return func(_ *Modification, t *Task) {
		t.Value = b
	}
}

// AppendingErr appends the given error to Err in the task.
func AppendingErr(e string) ChangeArg {
	return func(_ *Modification, t *Task) {
		var strs []string
		if t.Err != "" {
			strs = append(strs, t.Err)
		}
		if e != "" {
			strs = append(strs, e)
		}
		if len(strs) != 0 {
			t.Err = strings.Join(strs, "\n")
		}
	}
}

// ErrTo sets the Err field in the task.
func ErrTo(e string) ChangeArg {
	return func(_ *Modification, t *Task) {
		t.Err = e
	}
}

// ErrToZero sets the Err field to its zero value (clears the error).
func ErrToZero() ChangeArg {
	return ErrTo("")
}

// AttemptToNext sets the Attempt field in Task to the next value (increments it).
func AttemptToNext() ChangeArg {
	return func(_ *Modification, t *Task) {
		t.Attempt++
	}
}

// AttemptToZero resets the Attempt field to zero.
func AttemptToZero() ChangeArg {
	return func(_ *Modification, t *Task) {
		t.Attempt = 0
	}
}

// Change returns a ModifyArg that can be used in the Modify function, e.g.,
//
//	cli.Modify(ctx, task1.Change(ArrivalTimeBy(2 * time.Minute)))
func (t *Task) Change(args ...ChangeArg) ModifyArg {
	return func(m *Modification) {
		newTask := *t
		// From queue is always the current queue.
		newTask.FromQueue = t.Queue
		// Zero time signals the backend to use its own "now" and clear the
		// claimant (t is released). Callers may override via ArrivalTimeTo
		// or ArrivalTimeBy to renew or defer.
		newTask.At = time.Time{}
		for _, a := range args {
			a(m, &newTask)
		}
		m.Changes = append(m.Changes, &newTask)
	}
}

// Depend returns a ModifyArg that can be used to create a Modify dependency, e.g.,
//
//	cli.Modify(ctx, task.Depend())
func (t *Task) Depend() ModifyArg {
	return t.IDVersion().Depend()
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
	return t.Change(args...)
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
	return NewTaskID(t.ID, t.Version, t.Queue)
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
	newT.Value = append(json.RawMessage(nil), t.Value...)
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

// ValueAs unmarshals the task value into v. Same semantics as json.Unmarshal.
// For one-shot unmarshaling into a new value of a known type, prefer the
// package-level ValueAs[T] generic function.
func (t *Task) ValueAs(v any) error {
	return json.Unmarshal(t.Value, v)
}

// ValueAs unmarshals raw into a new value of type T and returns it.
//
//	spec, err := entroq.ValueAs[JobSpec](task.Value)
func ValueAs[T any](raw json.RawMessage) (T, error) {
	var v T
	if raw == nil {
		return v, nil
	}
	if rawV, ok := any(&v).(*json.RawMessage); ok {
		*rawV = raw
		return v, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, err
	}
	return v, nil
}

// GetValue unmarshals the task's value into a new value of type T and returns it.
// It is a convenience wrapper around ValueAs[T](task.Value).
func GetValue[T any](t *Task) (T, error) {
	if t == nil {
		var v T
		return v, fmt.Errorf("GetValue on nil task")
	}
	return ValueAs[T](t.Value)
}
