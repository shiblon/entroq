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
//
// # Doc Store
//
// In addition to tasks, EntroQ provides a key-value doc store that lives in
// the same atomic transaction space as tasks. A doc has a namespace, an
// auto-assigned ID, a primary key, an optional secondary key, a JSON content
// field, and an optional expiration time.
//
// Unlike tasks, docs are not work items to be claimed and deleted. They are
// durable shared state: configuration, counters, reduce output, or any data
// that multiple workers need to read or coordinate around.
//
// Docs and tasks share the same Modify call, so you can atomically create a
// task and its initial doc state, or delete a task and update a doc, in one
// round trip with no possibility of partial failure.
//
// # Doc Keys and Ordering
//
// The primary key groups related docs together. ClaimDocs acquires an
// exclusive lease on all docs sharing a primary key in one atomic operation,
// making the primary key the natural unit of exclusive ownership. The
// secondary key provides a sort dimension within that group.
//
// Both Docs and ClaimDocs return results ordered by (primary key, secondary
// key). This is guaranteed by both the PostgreSQL and eqmem backends. A
// worker that claims a primary-key group receives docs in stable secondary-key
// order, making reduce-shaped strategies simple.
//
// # Doc Storage Considerations
//
// All docs sharing a primary key are locked together during a claim. If a
// single primary key references many docs, claim operations over that key
// will be slower and take more memory proportional to the count. Design
// primary keys so that the set of docs under one key is reasonable.
//
// The eqmem backend performs O(n) range scans over the full namespace on every
// Docs or ClaimDocs call. It is suitable for development, testing, and
// low-volume production use when docs play a central role for you. For
// high-volume or latency-sensitive workloads, PostgreSQL is the recommended
// backend.
//
// # Task-as-Mutex Pattern
//
// When multiple workers might compete to claim the same primary key, the
// system will behave correctly, but contention can be reduced if documents are
// only altered by workers that hold a related task, making that task a sort of
// mutex. It could be a helpful design optimiation for some workloads,
// eliminating contention on ClaimDocs entirely - only the worker holding the
// task will ever try to claim those docs. It also bounds the lifetime of the
// claim: when the task is deleted in Finish, the logical lock is released.
//
// Example usage:
//
//  // Create a new doc
//  doc := entroq.Doc{
//      Namespace: "my_namespace",
//      Key: "my_key",
//      SecondaryKey: "my_secondary_key",
//      Content: json.RawMessage(`{"foo": "bar"}`),
//  }
//
//  // Insert the doc into the store
//  err := entroq.InsertDoc(ctx, doc)
//  if err != nil {
//      log.Fatal(err)
//  }
//
//  // Claim the doc
//  claimedDocs, err := entroq.ClaimDocs(ctx, "my_namespace", "my_key")
//  if err != nil {
//      log.Fatal(err)
//  }
//
//  // Update the doc
//  doc.Content = json.RawMessage(`{"foo": "baz"}`)
//  err = entroq.UpdateDoc(ctx, doc)
//  if err != nil {
//      log.Fatal(err)
//  }
//
//  // Delete the doc
//  err = entroq.DeleteDoc(ctx, "my_namespace", "my_key")
//  if err != nil {
//      log.Fatal(err)
//  }
//
package entroq

import (
    "encoding/json"
    "fmt"
    "log"
    "time"
)

// DocOpt is an option for doc creation or modification. Options that
// only apply to creation (WithKeys, WithIDKeys) are documented as such;
// passing them to Change has no effect.
type DocOpt func(*docOpts)

type docOpts struct {
    id string
    key string
    secondaryKey string
    content json.RawMessage
    at time.Time
    skipCollidingID bool
}

// WithKeys sets the primary and secondary keys for doc creation. The ID is
// auto-assigned. This option has no effect when passed to Change (keys are
// immutable after creation). Prefer this over WithIDKeys for normal use.
func WithKeys(key, secondary string) DocOpt {
    return func(o *docOpts) {
        o.key = key
        o.secondaryKey = secondary
    }
}

// WithIDKeys sets the ID and keys for doc creation. The ID is normally
// auto-assigned; only use this when you need explicit ID control, such as
// when replaying a journal, migrating data, or proxying through a gRPC
// service. This option has no effect when passed to Change (keys are
// immutable after creation).
func WithIDKeys(id, key, secondary string) DocOpt {
    return func(o *docOpts) {
        o.id = id
        o.key = key
        o.secondaryKey = secondary
    }
}

// WithRawContent sets the content payload of a doc.
func WithRawContent(val json.RawMessage) DocOpt {
    return func(o *docOpts) {
        o.content = val
    }
}

// WithContent sets the content payload of a doc, marshaling it to JSON first.
// This is a "must" function in the sense that the value must be marshalable.
// If this is a data value, it will always work. Things like channels and
// functions are what would trigger a fatal error here.
//
// Use WithRawContent for pre-marshaled data.
func WithContent(v any) DocOpt {
    b, err := json.Marshal(v)
    if err != nil {
        log.Fatalf("entroq doc: WithValue: %v", err)
    }
    return WithRawContent(b)
}

// WithDocArrivalTime sets the arrival time on a doc change. When non-zero and in the
// future, the backend will also record the caller as the claimant so the doc
// can be renewed or released.
func WithDocArrivalTime(t time.Time) DocOpt {
    return func(o *docOpts) {
        o.at = t
    }
}

// WithDocArrivalTimeBy sets the doc arrival time to now plus d. Use this to
// claim or renew a doc by pushing its At into the future.
func WithDocArrivalTimeBy(d time.Duration) DocOpt {
    return func(o *docOpts) {
        o.at = time.Now().Add(d)
    }
}

// WithSkipCollidingDoc marks a doc insert as skippable when its explicit ID
// already exists. When the caller specifies an ID and that doc is already
// present, the Modify call removes this insert and retries rather than
// returning an error. Analogous to WithSkipColliding for task inserts.
func WithSkipCollidingDoc(skip bool) DocOpt {
    return func(o *docOpts) {
        o.skipCollidingID = skip
    }
}

// DocID contains the identifying parts of a storage doc.
type DocID struct {
    Namespace string `json:"namespace"`
    ID string `json:"id"`
    Version int32 `json:"version"`
}

func (r DocID) String() string {
    return fmt.Sprintf("%s/%s:v%d", r.Namespace, r.ID, r.Version)
}

// Delete returns a ModifyArg that deletes the doc identified by this DocID.
func (r DocID) Delete() ModifyArg {
    return DeletingDocID(r.Namespace, r.ID, r.Version)
}

// Depend returns a ModifyArg that adds a version-pinned dependency on this DocID.
func (r DocID) Depend() ModifyArg {
    return DependingOnDocID(r.Namespace, r.ID, r.Version)
}

// DocData contains just the data portion of a storage doc, used for
// insertions and journal replay. Created and Modified are populated when
// journaling to preserve original timestamps on replay.
type DocData struct {
    Namespace string `json:"namespace"`
    ID string `json:"id"`
    Key string `json:"key"`
    SecondaryKey string `json:"secondary_key"`
    Content json.RawMessage `json:"content"`
    Created time.Time `json:"created"`
    Modified time.Time `json:"modified"`
    // skipCollidingID indicates that a collision on insertion is not fatal.
    // When the explicit ID already exists, Modify removes this insert and
    // retries rather than returning an error. Analogous to TaskData's field.
    skipCollidingID bool
}

// Doc represents a durable state record in EntroQ.
type Doc struct {
    Namespace string `json:"namespace"`
    ID string `json:"id"`
    Version int32 `json:"version"`
    Claimant string `json:"claimant"`
    At time.Time `json:"at"`
    Key string `json:"key"`
    SecondaryKey string `json:"secondary_key"`
    Content json.RawMessage `json:"content"`
    Created time.Time `json:"created"`
    Modified time.Time `json:"modified"`
}

// Data returns a DocData from this Doc, preserving timestamps for journaling.
func (r *Doc) Data() *DocData {
    rd := &DocData{
        Namespace: r.Namespace,
        ID: r.ID,
        Key: r.Key,
        SecondaryKey: r.SecondaryKey,
        Content: r.Content,
        Created: r.Created,
        Modified: r.Modified,
    }
    return rd
}
