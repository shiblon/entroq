package workgateway

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/shiblon/entroq"
)

// This file defines the entire worker-gateway wire protocol: every JSON message
// on the wire and the translation of a declarative modification into entroq
// modify arguments. It is deliberately small and self-describing so a worker can
// be written in any language without importing entroq, gRPC, or the queue API.
//
// # Registration is out-of-band
//
// The queues a worker serves, its max-attempts, and which phases it implements
// (takeDocs, work, cleanup) are connection-scoped and fixed for the session, so
// they are supplied at connection time out-of-band, not as a wire message: flags
// or env when a client spawns the gateway over a pipe, URL params or headers when
// a client opens a WebSocket. Those are the same idea (a connection preamble) in
// each transport, which keeps the transports alike. See workgateway.Config.
//
// # Lifecycle
//
// A connection is one worker slot: exactly one task in flight, strict
// request/response, no correlation ids (concurrency is more connections). Once
// connected the gateway drives a loop that mirrors the Go worker lifecycle,
// sending each phase message only if the worker registered for that phase:
//
//	  (gateway claims a task and begins renewing it)
//	gateway -> takeDocs {task}            # only if the worker registered takeDocs
//	client  -> docs {claims: [...]}
//	  (gateway claims the docs, sorted, and passes them along)
//	gateway -> doWork {task, docs}
//	client  -> result {outcome, modification?, ...}
//	  (gateway stops renewal, freezes the stable version, and commits atomically)
//	gateway -> cleanup {}                 # only if registered cleanup and outcome was ok
//	client  -> done {outcome?}
//	  (gateway claims the next task; loop)
//
// The commit is the exactly-once boundary. Everything up to it is at-least-once
// (a dropped connection reclaims the task on lease expiry); cleanup after it is
// best-effort and at-most-once, exactly like the Go worker's OnSuccess hook. No
// framework can make a post-commit external effect exactly-once without a
// two-phase commit, so cleanup must be idempotent or safe to skip.

// Message type tags. Every protocol message is a JSON object with a "type".
const (
	msgTakeDocs = "takeDocs"
	msgDocs     = "docs"
	msgDoWork   = "doWork"
	msgResult   = "result"
	msgCleanup  = "cleanup"
	msgDone     = "done"
)

// Outcomes a client reports for a task (result.Outcome) or for cleanup
// (done.Outcome). They map one-to-one onto the Go worker's dispositions, so a
// wire worker has exactly the vocabulary a native one does and nothing more.
const (
	outcomeOK    = "ok"    // no error; commit result.Modification (which may be empty)
	outcomeRetry = "retry" // re-queue with backoff, quarantining once attempts exhaust
	outcomeMove  = "move"  // send straight to a destination (error) queue
	outcomeFatal = "fatal" // stop the whole worker
)

// takeDocsMsg asks the client which docs the claimed task needs. The doc set is
// a function of the task, so it cannot be static config; it has to be a
// callback. Sent only when the client registered takeDocs.
type takeDocsMsg struct {
	Type string       `json:"type"`
	Task *entroq.Task `json:"task"`
}

// docsMsg is the client's reply to takeDocs: the docs to claim, each identified
// by namespace and key. The gateway claims them (sorted by namespace,key to
// avoid dining-philosopher livelock) before sending doWork.
type docsMsg struct {
	Type   string     `json:"type"`
	Claims []docClaim `json:"claims"`
}

// docClaim names one doc to acquire for the task.
type docClaim struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
}

// doWorkMsg carries the task and any acquired docs to the client for the actual
// work. Task is the identical entroq.Task a native DoWork would receive (Value
// stays raw JSON), so a wire worker sees exactly what an in-process one does,
// down to fields like FromQueue and Attempt a reaper or authorizer might use.
type doWorkMsg struct {
	Type string        `json:"type"`
	Task *entroq.Task  `json:"task"`
	Docs []*entroq.Doc `json:"docs,omitempty"`
}

// result is the client's reply to doWork: exactly one outcome. On "ok" the
// gateway commits Modification; an absent or empty modification commits nothing
// and leaves the task to be reclaimed after its lease, faithful to a Go DoModify
// that returns no mods. "retry"/"move"/"fatal" carry the same knobs the Go
// sentinels do.
type result struct {
	Type         string        `json:"type"`
	Outcome      string        `json:"outcome"`
	Modification *modification `json:"modification,omitempty"`
	Message      string        `json:"message,omitempty"` // retry/move/fatal detail
	After        string        `json:"after,omitempty"`   // retry: delay before re-arrival, e.g. "30s"
	OrMove       string        `json:"orMove,omitempty"`  // retry: quarantine queue once attempts exhaust
	To           string        `json:"to,omitempty"`      // move: destination queue
}

// cleanupMsg tells the client the commit succeeded and it may run its
// post-commit step. It carries no payload: the client holds whatever state it
// computed during work in its own process, across the round trip.
type cleanupMsg struct {
	Type string `json:"type"`
}

// done is the client's reply to cleanup. Cleanup is best-effort, so "ok" (or a
// bare {"type":"done"}) continues the loop and only "fatal" stops the worker.
// There is deliberately no retry/move here: the task already committed, so there
// is nothing left to retry or move (the same rule the Go OnSuccess doc states).
type done struct {
	Type    string `json:"type"`
	Outcome string `json:"outcome,omitempty"`
	Message string `json:"message,omitempty"`
}

// modification mirrors the public shape of entroq.Modification: lists of task
// operations applied atomically as one Modify. Deletion is never implied by a
// successful outcome; a client that wants to consume the input task lists it in
// Deletes, exactly as a Go worker returns task.Delete().
//
// A change carries the full task rather than a delta (see change), which is how
// it preserves the fields the client does not restate and keeps a task's ID
// stable across a move. The Doc* lists give docs the identical four operations,
// so a wire worker has the same modification vocabulary an in-process DoModify
// worker does: insert, change, delete, depend on both tasks and docs.
type modification struct {
	Inserts []insertArg `json:"inserts,omitempty"`
	Changes []change    `json:"changes,omitempty"`
	Deletes []taskRef   `json:"deletes,omitempty"`
	Depends []taskRef   `json:"depends,omitempty"`

	DocInserts []docInsert `json:"docInserts,omitempty"`
	DocChanges []docChange `json:"docChanges,omitempty"`
	DocDeletes []docRef    `json:"docDeletes,omitempty"`
	DocDepends []docRef    `json:"docDepends,omitempty"`
}

// insertArg inserts a new task. Only Queue is required: omit Value for an empty
// value, omit ID to let the backend assign one, omit At for immediate
// availability. At is an absolute time (RFC 3339 on the wire); a client that
// wants "in N seconds" computes the absolute moment itself.
type insertArg struct {
	Queue string          `json:"queue"`
	Value json.RawMessage `json:"value,omitempty"`
	ID    string          `json:"id,omitempty"`
	At    *time.Time      `json:"at,omitempty"`
}

// change updates an existing task. It carries the full base Task (the client
// echoes the task it received in doWork, which is why any field it does not
// restate keeps its current value) plus the deltas to apply. The claimed task's
// version is fixed up by the gateway to the stable renewed value, so echoing the
// version seen in doWork is fine.
//
//   - ToQueue moves the task, preserving its ID and bumping only its version
//     (EntroQ's move semantics).
//   - At sets a new arrival time; omitting it keeps the task's current At rather
//     than releasing the task to "now" (the raw entroq default for a change).
//   - Value replaces the task's value; omitting it keeps the current value.
type change struct {
	Task    *entroq.Task    `json:"task"`
	ToQueue string          `json:"toQueue,omitempty"`
	At      *time.Time      `json:"at,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
}

// taskRef identifies a task for a delete or a depend. Queue is optional metadata
// used for change/move authorization.
type taskRef struct {
	ID      string `json:"id"`
	Version int32  `json:"version"`
	Queue   string `json:"queue,omitempty"`
}

// docInsert inserts a new doc. Namespace and Key are required; omit ID to let the
// backend assign one, omit Content for an empty doc.
type docInsert struct {
	Namespace    string          `json:"namespace"`
	Key          string          `json:"key"`
	SecondaryKey string          `json:"secondaryKey,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"`
	ID           string          `json:"id,omitempty"`
}

// docChange updates an existing doc. Like a task change it carries the full base
// Doc (the client echoes the doc it received in doWork), so an unrestated field
// keeps its value. Only Content and At are mutable; a doc's namespace and keys
// are immutable. The claimed doc's version is fixed up to the renewed value.
type docChange struct {
	Doc     *entroq.Doc     `json:"doc"`
	Content json.RawMessage `json:"content,omitempty"`
	At      *time.Time      `json:"at,omitempty"`
}

// docRef identifies a doc for a delete or a depend.
type docRef struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
	Version   int32  `json:"version"`
}

// modifyArgs translates a wire modification into entroq modify arguments. It is
// the single place the gateway maps the language-agnostic protocol onto the Go
// API, which is exactly why a client never has to import entroq. A nil
// modification is a valid empty commit.
func (m *modification) modifyArgs() ([]entroq.ModifyArg, error) {
	if m == nil {
		return nil, nil
	}
	var args []entroq.ModifyArg
	for _, ins := range m.Inserts {
		if ins.Queue == "" {
			return nil, fmt.Errorf("insert: queue is required")
		}
		var opts []entroq.InsertArg
		if ins.Value != nil {
			opts = append(opts, entroq.WithRawValue(ins.Value))
		}
		if ins.ID != "" {
			opts = append(opts, entroq.WithID(ins.ID))
		}
		if ins.At != nil {
			opts = append(opts, entroq.WithArrivalTime(*ins.At))
		}
		args = append(args, entroq.InsertingInto(ins.Queue, opts...))
	}
	for _, ch := range m.Changes {
		if ch.Task == nil || ch.Task.ID == "" {
			return nil, fmt.Errorf("change: a task with an id is required")
		}
		opts := []entroq.ChangeArg{}
		if ch.ToQueue != "" {
			opts = append(opts, entroq.QueueTo(ch.ToQueue))
		}
		// A raw entroq change releases the task to "now"; preserve the base
		// arrival time unless the client explicitly sets a new one.
		if ch.At != nil {
			opts = append(opts, entroq.ArrivalTimeTo(*ch.At))
		} else {
			opts = append(opts, entroq.ArrivalTimeTo(ch.Task.At))
		}
		if ch.Value != nil {
			opts = append(opts, entroq.RawValueTo(ch.Value))
		}
		args = append(args, entroq.Changing(ch.Task, opts...))
	}
	for _, d := range m.Deletes {
		if d.ID == "" {
			return nil, fmt.Errorf("delete: id is required")
		}
		args = append(args, entroq.Deleting(d.ID, d.Version, entroq.WithIDQueue(d.Queue)))
	}
	for _, d := range m.Depends {
		if d.ID == "" {
			return nil, fmt.Errorf("depend: id is required")
		}
		args = append(args, entroq.DependingOn(d.ID, d.Version, entroq.WithIDQueue(d.Queue)))
	}
	for _, di := range m.DocInserts {
		if di.Namespace == "" || di.Key == "" {
			return nil, fmt.Errorf("docInsert: namespace and key are required")
		}
		args = append(args, entroq.InsertingDoc(&entroq.DocData{
			Namespace:    di.Namespace,
			ID:           di.ID,
			Key:          di.Key,
			SecondaryKey: di.SecondaryKey,
			Content:      di.Content,
		}))
	}
	for _, dc := range m.DocChanges {
		if dc.Doc == nil || dc.Doc.ID == "" {
			return nil, fmt.Errorf("docChange: a doc with an id is required")
		}
		var opts []entroq.DocOpt
		if dc.Content != nil {
			opts = append(opts, entroq.WithRawContent(dc.Content))
		}
		if dc.At != nil {
			opts = append(opts, entroq.WithDocArrivalTime(*dc.At))
		}
		args = append(args, dc.Doc.Change(opts...))
	}
	for _, dd := range m.DocDeletes {
		if dd.ID == "" {
			return nil, fmt.Errorf("docDelete: id is required")
		}
		args = append(args, entroq.DeletingDocID(dd.Namespace, dd.ID, dd.Version))
	}
	for _, dd := range m.DocDepends {
		if dd.ID == "" {
			return nil, fmt.Errorf("docDepend: id is required")
		}
		args = append(args, entroq.DependingOnDocID(dd.Namespace, dd.ID, dd.Version))
	}
	return args, nil
}
