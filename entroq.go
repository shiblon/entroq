// Package entroq defines an atomic task lease manager as described in depth at
// https://github.com/shiblon/entroq and associated wiki pages.
// You can get the docker image at https://hub.docker.com/shiblon/entroq.
//
// The gist: if you have a bunch of stuff that needs to get done and you don't
// want to lose any of it to failures, and you don't want to commit any of it
// twice, you want this. It's fault-tolerant, it won't ever repeat itself, and
// it makes progress even when some task data causes persistent issues; other
// tasks will still progress.
//
// In other words, it's a fault-tolerant competing consumer workqueue system
// with exactly-once semantics, progress guarantees, and strong atomicity.
// Inspired by Google's Spanner Queues, but free and very cheap to run yourself.
// The in-memory implementation uses a journal that replays extremely quickly,
// so failures are really just fast-recovery events. I've literally had network
// switch lose power in a data center cage, causing all the workers to start
// crash-looping; then with power restored and no other intervention,
// everything just started moving again with no work lost or repeated.
//
// The PostgreSQL implementation is pure stored procedures and LISTEN/NOTIFY, so
// if you want, you don't even need all of this. You can do everything with the
// schema file and some scripting. No additional server protocols, just use
// PostgreSQL native privileges and connections. Or you can use the nicer
// client approaches here, with workers, etc. In any case, see the Python pg
// client implementation for a thin wrapper around Postgres for inspiration.
//
// Using the Go implementation opens up possibilities of, among other things, an
// in-memory backend served via gRPC with queue-level authorization. To use
// GRPC as a service protocol, see cmd/eqpgsvc or cmd/eqmemsvc to start it up,
// and use cmd/eqc as a client to play with it.
//
// All of this is very lightweight. You can run it on a laptop or a cluster, it
// scales effortlessly. If you want to work faster, just start more workers.
// There's no configuration to fiddle with; the service just adapts because
// that's fundamental to the nature of a true competing consumer workflow
// system.
package entroq

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

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"
)

const (
	DefaultClaimPollTime = 30 * time.Second
	DefaultClaimDuration = 30 * time.Second
)

// ClaimQuery contains information necessary to attempt to make a claim on a task in a specific queue.
type ClaimQuery struct {
	Queues   []string      // Queues to attempt to claim from. Only one wins.
	Claimant string        // The ID of the process trying to make the claim.
	Duration time.Duration // How long the task should be claimed for if successful.
	PollTime time.Duration // Length of time between (possibly interruptible) sleep and polling.
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
			return nil, fmt.Errorf("poll try claim: %w", err)
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
			return nil, fmt.Errorf("poll try claim: %w", ctx.Err())
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
	qs := make(map[string]string)
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
		return nil, fmt.Errorf("wait try claim: %w", err)
	}
	if condErr != nil {
		return nil, fmt.Errorf("wait try claim condition: %w", condErr)
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

// IDGenerator is a function that can generate IDs. Used for generating task IDs
// and claimant IDs when they are not specified. By default, this is
// Hex16Generator NOTE: any ID generated must be <= 64 characters in length for
// some backends.
type IDGenerator func() string

// EntroQ is the primary client interface for interacting with the task queue backend.
// It manages task IDs, claimant IDs, and the underlying storage connection.
type EntroQ struct {
	backend   Backend
	opener    BackendOpener
	ClientID  string
	idGenRand IDGenerator
}

// Option is an option that modifies how EntroQ clients are created.
type Option func(*EntroQ)

// WithClaimantID sets the default claimaint ID for this client.
func WithClaimantID(id string) Option {
	return func(eq *EntroQ) {
		eq.ClientID = id
	}
}

// Hex16Generator is an ID generator that produces 16-character random
// hex strings. It is the default for New clients.
func Hex16Generator() string {
	return fmt.Sprintf("%016x", rand.Uint64())
}

// WithIDGenerator sets the default ID generator, which affects task IDs and
// claimant IDs when autogenerated. By default, this will be Hex16Generator.
func WithIDGenerator(gen IDGenerator) Option {
	return func(eq *EntroQ) {
		eq.idGenRand = gen
	}
}

// WithBackend sets the backend for this client. Note that if WithBackendOpener
// is also specified, this will be used and the opener will be ignored.
func WithBackend(backend Backend) Option {
	return func(eq *EntroQ) {
		eq.backend = backend
	}
}

// BackendOpener is a function that can open a connection to a backend. Creating
// a client with a specific backend is accomplished by passing one of these functions
// into New.
type BackendOpener func(ctx context.Context) (Backend, error)

// New creates a new task client with the given backend implementation.
//
// By default, it uses Hex16Generator for task and claimant IDs and assigns
// a unique random claimant ID to this client instance.
//
// Example using an in-memory implementation:
//
//	cli, err := New(ctx, eqmem.Opener())
//
// This opens a new EntroQ instance using the in-memory backend. Each backend
// has its own set of options that can be passed to its opener. For example, the
// in-memory backend can be configured to use a fault-tolerant fast journal for
// durable storage and fault recovery.
//
// Backends available are
//   - eqmem: in-memory, optionally journal-backed (recommended) - fast and lean
//   - eqpg: PostgreSQL-backed - persistent, robust, supports mixing database operations
//     with EntroQ operations in the same transaction. Clients limited by PG connections.
//   - eqgrpc: gRPC client backend - connects to a remote EntroQ service that is
//     backed by eqmem or eqpg (or similar direct stores). Requires you to start or
//     point to a service. Can be started with cmd/eqpg
//
// Valid backends will be in pkg/backend
func New(ctx context.Context, opener BackendOpener, opts ...Option) (*EntroQ, error) {
	eq := &EntroQ{
		idGenRand: Hex16Generator,
	}
	for _, o := range opts {
		o(eq)
	}
	if eq.backend != nil {
		eq.opener = nil
	} else if opener != nil {
		eq.opener = opener
		var err error
		if eq.backend, err = opener(ctx); err != nil {
			return nil, fmt.Errorf("backend opener: %w", err)
		}
	} else {
		return nil, fmt.Errorf("no backend or backend opener specified")
	}
	eq.ClientID = eq.idGenRand()
	return eq, nil
}

// Close closes the underlying backend.
func (c *EntroQ) Close() error {
	return c.backend.Close()
}

// ID returns the default claimant ID of this client. Used in "bare" calls,
// like Modify, Claim, etc. To change the ID per call (usually not needed, and
// can be dangerous), use the "As" calls, e.g., ModifyAs.
func (c *EntroQ) ID() string {
	return c.ClientID
}

// GenID creates a new ID using the client's ID generator.
func (c *EntroQ) GenID() string {
	return c.idGenRand()
}

// ClaimOpt modifies limits on a task claim.
type ClaimOpt func(*ClaimQuery)

// ClaimAs sets the claimant ID for a claim operation. When not set, uses the internal default for this client.
func ClaimAs(id string) ClaimOpt {
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
// gRPC backends translate codes.Canceled to context.Canceled at the boundary,
// so this only needs to check native Go errors.
func IsCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// IsTimeout indicates whether the error is a timeout error.
// gRPC backends translate codes.DeadlineExceeded to context.DeadlineExceeded
// at the boundary, so this only needs to check native Go errors.
func IsTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

// claimQueryFromOpts processes ClaimOpt values and produces a claim query.
func claimQueryFromOpts(claimant string, opts ...ClaimOpt) *ClaimQuery {
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
	query := claimQueryFromOpts(c.ClientID, opts...)
	if len(query.Queues) == 0 {
		return nil, fmt.Errorf("no queues specified for claim")
	}
	return c.backend.Claim(ctx, query)
}

// TryClaim attempts one time to claim a task from the given queues. If
// there are no tasks, it returns a nil error *and* a nil task. This allows the
// caller to decide whether to retry. It can fail if certain (optional)
// dependency tasks are not present. This can be used, for example, to ensure
// that configuration tasks haven't changed.
func (c *EntroQ) TryClaim(ctx context.Context, opts ...ClaimOpt) (*Task, error) {
	query := claimQueryFromOpts(c.ClientID, opts...)
	if len(query.Queues) == 0 {
		return nil, fmt.Errorf("no queues specified for try claim")
	}
	return c.backend.TryClaim(ctx, query)
}

// RenewFor attempts to renew the given task's lease (update arrival time) for
// the given duration. Returns the new task.
func (c *EntroQ) RenewFor(ctx context.Context, task *Task, duration time.Duration) (*Task, error) {
	changed, err := c.RenewAllFor(ctx, []*Task{task}, duration)
	if err != nil {
		return nil, fmt.Errorf("renew task: %w", err)
	}
	return changed[0], nil
}

// RenewAllFor attempts to renew all given tasks' leases (update arrival times)
// for the given duration. Returns the new tasks.
func (c *EntroQ) RenewAllFor(ctx context.Context, tasks []*Task, duration time.Duration) (result []*Task, err error) {
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
	_, changed, err := c.Modify(ctx, modArgs...)
	if err != nil {
		return nil, fmt.Errorf("renewal failed for tasks %q: %w", taskIDs, err)
	}
	if len(changed) != len(tasks) {
		return nil, fmt.Errorf("renewal expected %d updated tasks, got %d", len(tasks), len(changed))
	}
	return changed, nil
}

// Modify allows a batch of mutations to be applied atomically to the task store.
//
// Every operation in a Modify call — whether it is an insertion, deletion,
// update, or dependency check — is gated on the existence and version of the
// specified tasks. If any part of the modification fails (e.g., a task has been
// claimed by another worker and its version has changed), the entire batch
// rolls back and a DependencyError is returned.
//
// This method is the foundation of EntroQ's "Exactly Once" semantics. By
// including a task deletion in the same Modify call as a result insertion,
// you ensure that work is never lost or committed twice.
//
// Returns:
//   - inserted: The tasks that were successfully created.
//   - changed: The tasks that were successfully updated.
//   - err: A *DependencyError if any version check failed.
func (c *EntroQ) Modify(ctx context.Context, modArgs ...ModifyArg) (inserted []*Task, changed []*Task, err error) {
	mod := NewModification(c.ClientID, modArgs...)
	// Generate IDs for any inserts that don't have them. This is necessary
	// because the backend does not assign IDs.
	for _, ins := range mod.Inserts {
		if ins.ID == "" {
			ins.ID = c.GenID()
		}
	}
	for _, opt := range mod.Options() {
		if err := opt.IsModifyBackend(c.backend); err != nil {
			return nil, nil, fmt.Errorf("modify option incompatible with backend %T: %w", c.backend, err)
		}
	}
	for _, ins := range mod.Inserts {
		if ins.Value != nil && !json.Valid(ins.Value) {
			return nil, nil, fmt.Errorf("modify insert %q: value is not valid JSON", ins.Queue)
		}
	}
	for _, chg := range mod.Changes {
		if chg.Value != nil && !json.Valid(chg.Value) {
			return nil, nil, fmt.Errorf("modify change %s: value is not valid JSON", chg.ID)
		}
	}
	for {
		ins, chg, err := c.backend.Modify(ctx, mod)
		if err == nil {
			return ins, chg, nil
		}
		depErr, ok := AsDependency(err)
		// If not a dependency error, pass it on out.
		if !ok {
			return nil, nil, fmt.Errorf("modify: %w", err)
		}
		// If anything is missing or there's a claim problem, bail.
		if depErr.HasMissing() || depErr.HasClaims() {
			return nil, nil, fmt.Errorf("modify non-ins deps: %w", err)
		}
		// No collisions? Not sure what's going on.
		if !depErr.HasCollisions() {
			return nil, nil, fmt.Errorf("non-collision errors: %w", err)
		}

		// If we get this far, the only errors we have are insertion collisions.
		// If any of them cannot be skipped, we bail out.
		// Otherwise we remove them and try again.

		// Now check all collisions. If we can remove all of them safely, do so
		// and try again.
		collidingIDs := make(map[string]bool)
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
				return nil, nil, fmt.Errorf("unskippable collision: %w", err)
			}
			newInserts = append(newInserts, td)
		}
		mod.Inserts = newInserts
	}
}

// ModifyArg is an argument to the Modify function, which does batch modifications to the task store.
type ModifyArg func(m *Modification)

// ModifyAs sets the claimant ID for a particular modify call. Usually not
// needed, can be dangerous unless used with extreme care. The client default
// is used if this option is not provided.
func ModifyAs(id string) ModifyArg {
	return func(m *Modification) {
		m.Claimant = id
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
// Or, better still,
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

// ModifyOption is an option that can be passed through to a backend's Modify
// implementation. Backend-specific options use IsModifyBackend to validate that
// the correct backend is in use; generic options return nil unconditionally.
type ModifyOption interface {
	// IsModifyBackend returns nil if the given backend supports this option, or a
	// descriptive error if not. Returning non-nil causes Modify to fail before
	// the backend is called. Generic options always return nil.
	IsModifyBackend(Backend) error
}

// WithModifyOption allows an option the backend understands to be added.
// These are accessible in the backend if additional modification settings
// are needed (example: modifying inside an SQL transaction).
func WithModifyOption(opt ModifyOption) ModifyArg {
	return func(m *Modification) {
		m.options = append(m.options, opt)
	}
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

// Deleting adds a deletion to a Modify call, e.g.,
//
//	cli.Modify(ctx, Deleting(id, version))
func Deleting(id string, version int32, opts ...IDOption) ModifyArg {
	return func(m *Modification) {
		m.Deletes = append(m.Deletes, NewTaskID(id, version, opts...))
	}
}

// DependingOn adds a dependency to a Modify call, e.g., to insert a task into
// "my queue" with data "hey", but only succeeding if a task with anotherID and
// someVersion exists:
//
//	cli.Modify(ctx,
//	  InsertingInto("my queue",
//	    WithValue(json.RawMessage(`"hey"`))),
//	    DependingOn(anotherID, someVersion))
func DependingOn(id string, version int32, opts ...IDOption) ModifyArg {
	return func(m *Modification) {
		m.Depends = append(m.Depends, NewTaskID(id, version, opts...))
	}
}

// Changing adds a task update to a Modify call, e.g., to modify
// the queue a task belongs in:
//
//	cli.Modify(ctx, Changing(myTask, QueueTo("a different queue name")))
func Changing(task *Task, changeArgs ...ChangeArg) ModifyArg {
	return func(m *Modification) {
		newTask := *task
		// From queue is always the current queue.
		newTask.FromQueue = task.Queue
		// Reset when changing, at least by default. Callers may override.
		newTask.At = m.now
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
//	  cli.Modify(ctx,
//	    Changing(myTask,
//	      QueueTo("a new queue"),
//		  ArrivalTimeBy(5 * time.Minute)))
type ChangeArg func(m *Modification, t *Task)

// QueueTo creates an option to modify a task's queue in the Changing function.
func QueueTo(q string) ChangeArg {
	return func(_ *Modification, t *Task) {
		// Save the old queue for authorization to move this from one to another.
		t.FromQueue = t.Queue
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
	now     time.Time
	options []ModifyOption

	Claimant string `json:"claimant"`

	Inserts []*TaskData `json:"inserts"`
	Changes []*Task     `json:"changes"`
	Deletes []*TaskID   `json:"deletes"`
	Depends []*TaskID   `json:"depends"`
}

// String produces a friendly version of this modification.
func (m *Modification) String() string {
	var out []string
	if len(m.Inserts) != 0 {
		out = append(out, fmt.Sprintf("ins: %v", m.Inserts))
	}
	if len(m.Changes) != 0 {
		out = append(out, fmt.Sprintf("chg: %v", m.Changes))
	}
	if len(m.Deletes) != 0 {
		out = append(out, fmt.Sprintf("del: %v", m.Deletes))
	}
	if len(m.Depends) != 0 {
		out = append(out, fmt.Sprintf("dep: %v", m.Depends))
	}
	return "mod:\n\t" + strings.Join(out, "\n\t")
}

// NewModification creates a new modification: insertions, deletions, changes,
// and dependencies all together. When creating this for the purpose of passing
// to WithModification, set the claimant to empty (it is ignored in that case).
func NewModification(claimant string, modArgs ...ModifyArg) *Modification {
	m := &Modification{
		Claimant: claimant,
		now:      time.Now(),
	}
	for _, arg := range modArgs {
		arg(m)
	}
	return m
}

// Options returns a slice of ModifyOption, which backends can use to
// change how an individual call to Modify operates.
func (m *Modification) Options() []ModifyOption {
	return m.options
}

// modDependencies returns a dependency map for all modified dependencies
// (deletes and changes).
func (m *Modification) modDependencies() (map[string]int32, error) {
	deps := make(map[string]int32)
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

// AllDependencies returns a dependency map from ID to version for tasks that
// must exist and match version numbers. It returns an error if there are
// duplicates. Changes, Deletions, Dependencies and Insertions with IDs must be
// disjoint sets.
//
// When using this to query backend storage for the presence of tasks, note
// that it is safe to ignore the version if you use the DependencyError method
// to determine whether a transaction can proceed. That checks versions
// appropriate for all different types of modification.
func (m *Modification) AllDependencies() (map[string]int32, error) {
	deps, err := m.modDependencies()
	if err != nil {
		return nil, fmt.Errorf("get dependencies: %w", err)
	}
	for _, t := range m.Depends {
		if _, ok := deps[t.ID]; ok {
			return nil, fmt.Errorf("duplicates found in dependencies")
		}
		deps[t.ID] = t.Version
	}
	for _, t := range m.Inserts {
		// Add to dependencies (they need to *not* exist) if an ID is set.
		if t.ID == "" {
			continue
		}
		if _, ok := deps[t.ID]; ok {
			return nil, fmt.Errorf("duplicates found in dependencies")
		}
		deps[t.ID] = -1
	}
	return deps, nil
}

func (m *Modification) otherwiseClaimed(task *Task) bool {
	return task.At.After(m.now) && task.Claimant != "" && task.Claimant != m.Claimant
}

func (m *Modification) badInserts(foundDeps map[string]*Task) []*TaskID {
	var present []*TaskID
	for _, t := range m.Inserts {
		if found := foundDeps[t.ID]; found != nil {
			present = append(present, found.IDVersion())
		}
	}
	return present
}

func (m *Modification) badChanges(foundDeps map[string]*Task) (missing []*TaskID, claimed []*TaskID) {
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

func (m *Modification) badDeletes(foundDeps map[string]*Task) (missing []*TaskID, claimed []*TaskID) {
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

func (m *Modification) missingDepends(foundDeps map[string]*Task) []*TaskID {
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
func (m *Modification) DependencyError(found map[string]*Task) error {
	presentInserts := m.badInserts(found)
	missingChanges, claimedChanges := m.badChanges(found)
	missingDeletes, claimedDeletes := m.badDeletes(found)
	missingDepends := m.missingDepends(found)

	if len(presentInserts) > 0 || len(missingChanges) > 0 || len(claimedChanges) > 0 || len(missingDeletes) > 0 || len(claimedDeletes) > 0 || len(missingDepends) > 0 {
		return &DependencyError{
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
func DependencyErrorf(msg string, vals ...any) *DependencyError {
	return &DependencyError{Message: fmt.Sprintf(msg, vals...)}
}

// Copy produces a new deep copy of this error.
func (m *DependencyError) Copy() *DependencyError {
	e := &DependencyError{
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
func (m *DependencyError) HasMissing() bool {
	return len(m.Depends) > 0 || len(m.Deletes) > 0 || len(m.Changes) > 0
}

// HasClaims indicates whether any of the tasks were claimed by another claimant and unexpired.
func (m *DependencyError) HasClaims() bool {
	return len(m.Claims) > 0
}

// HasCollisions indicates whether any of the inserted tasks collided with existing IDs.
func (m *DependencyError) HasCollisions() bool {
	return len(m.Inserts) > 0
}

// OnlyClaims indicates that the error was only related to claimants. Useful
// for backends to do "force" operations, making it easy to ignore this
// particular error.
func (m *DependencyError) OnlyClaims() bool {
	return !m.HasMissing() && !m.HasCollisions()
}

// Error produces a helpful error string indicating what was missing.
func (m *DependencyError) Error() string {
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

// AsDependency returns the *DependencyError within err (if any), unwrapping
// through the error chain. Returns nil and false if err is not a DependencyError.
func AsDependency(err error) (*DependencyError, bool) {
	var derr *DependencyError
	if ok := errors.As(err, &derr); ok {
		return derr, true
	}
	return nil, false
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
