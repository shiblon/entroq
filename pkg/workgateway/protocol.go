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
// Two categories are deliberately reserved for a follow-up and are not
// represented here yet:
//
//   - Task CHANGES. A Go change copies the full task and applies deltas, so
//     preserving the fields a client does not restate needs the task's current
//     state, which the wire ref (id, version, queue) does not carry. Getting the
//     semantics right (delta vs full-state; the claimed task, whose state the
//     gateway holds, vs an arbitrary task) is its own design question. Until
//     then a move is expressible as delete + insert.
//   - Doc modifications (insert/change/delete/depend on docs). Doc CLAIMS
//     already work via the takeDocs phase; writing docs back does not yet.
//
// See the WIP note in the wiki proposal.
type modification struct {
	Inserts []insertArg `json:"inserts,omitempty"`
	Deletes []taskRef   `json:"deletes,omitempty"`
	Depends []taskRef   `json:"depends,omitempty"`
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

// taskRef identifies a task for a delete or a depend. Queue is optional metadata
// used for change/move authorization.
type taskRef struct {
	ID      string `json:"id"`
	Version int32  `json:"version"`
	Queue   string `json:"queue,omitempty"`
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
	return args, nil
}
