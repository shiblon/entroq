package workgateway

import (
	"fmt"
	"time"

	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/worker"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// This file defines the entire worker-gateway wire protocol: every message on
// the wire and the vocabulary of task dispositions a worker may choose. It is
// deliberately small and self-describing so a worker can be written in any
// language without importing entroq, gRPC, or the queue API.
//
// # The payloads are the EntroQ protobufs, as protojson
//
// A worker never hand-models a task, a doc, or a modification. Every domain
// object on the wire is the canonical protojson form of the corresponding
// message in api/entroq.proto (pb.Task, pb.Doc, pb.ModifyRequest, pb.ModifyDep),
// so a foreign worker generates those types from the same proto the gRPC client
// and service use and gets the identical schema for free. Only the thin envelope
// around them -- the "type" discriminator and the phase framing below -- is
// gateway-specific and hand-defined; the wireX wrappers below are what marshal
// the embedded protobuf as protojson rather than as a Go struct, so the envelope
// can ride over an ordinary JSON transport while its payloads stay canonical.
//
// A worker does not fill in a modification's claimant_id: the gateway owns the
// claim, so it attributes the commit itself and ignores whatever the worker sent
// there (see entroq.WithModification).
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
//	client  -> result {outcome, ack?, modification?, ...}
//	  (gateway stops renewal, freezes the stable version, and commits atomically)
//	  (then exactly ONE post-commit phase fires, and only if the worker registered it)
//	gateway -> success {}                 # the commit succeeded
//	client  -> done {outcome}             #   ok continues; fatal stops; retry/move are no-ops
//	gateway -> dependency {deps: [...]}   # the commit lost a dependency race
//	client  -> done {outcome}             #   ok reclaims on lease; or a retry/move/fatal sentinel
//	  (gateway claims the next task; loop)
//
// The two post-commit phases (success, dependency) are mutually exclusive and
// each opt-in: exactly one can fire, and only if the worker registered it. They
// are named by the outcome that triggers them, unlike the pre-commit phases
// (takeDocs, work) which are named by the action the worker performs -- the
// naming basis flips at the commit boundary because before it the worker is
// asked what to do, and after it it is told what happened.
//
// The commit is the exactly-once boundary. Everything up to it is at-least-once
// (a dropped connection reclaims the task on lease expiry); the success phase
// after it is best-effort and at-most-once, exactly like the Go worker's
// OnSuccess hook. No framework can make a post-commit external effect
// exactly-once without a two-phase commit, so success-phase work must be
// idempotent or safe to skip.
//
// The dependency phase runs only when the commit *fails* a dependency check (a
// depended-on task or doc was scooped or vanished). The gateway reports exactly
// which dependencies failed and lets the worker pick the task's disposition, the
// same choice the Go worker's OnDependency hook makes.

// Message type tags. Every protocol message is a JSON object with a "type".
const (
	msgTakeDocs   = "takeDocs"
	msgDocs       = "docs"
	msgDoWork     = "doWork"
	msgResult     = "result"
	msgSuccess    = "success"
	msgDependency = "dependency"
	msgDone       = "done"
)

// Outcomes a client reports for a task (result.Outcome, ack.Outcome) or for
// cleanup (done.Outcome). They map one-to-one onto the Go worker's dispositions,
// so a wire worker has exactly the vocabulary a native one does and nothing more.
const (
	outcomeOK    = "ok"    // no error; commit result.Modification (which may be empty)
	outcomeRetry = "retry" // re-queue with backoff, quarantining once attempts exhaust
	outcomeMove  = "move"  // send straight to a destination (error) queue
	outcomeFatal = "fatal" // stop the whole worker
)

// The wireX types carry an api/entroq.proto message inside a JSON envelope as
// canonical protojson. Embedding the pointer keeps field access transparent
// (msg.Task.Value), while the marshal methods override encoding/json's default
// struct handling -- which would emit non-canonical field names, int64s as
// numbers, and enums as integers -- with the protojson a foreign worker's
// generated types expect. MarshalJSON is on the value receiver so both direct
// fields and slice elements encode; UnmarshalJSON is on the pointer so the
// decoder can allocate the message.

// wireTask is a pb.Task on the wire.
type wireTask struct{ *pb.Task }

// MarshalJSON renders the task as protojson.
func (w wireTask) MarshalJSON() ([]byte, error) { return protojson.Marshal(w.Task) }

// UnmarshalJSON reads a protojson task.
func (w *wireTask) UnmarshalJSON(b []byte) error {
	w.Task = &pb.Task{}
	return unmarshalPB(b, w.Task)
}

// wireDoc is a pb.Doc on the wire.
type wireDoc struct{ *pb.Doc }

// MarshalJSON renders the doc as protojson.
func (w wireDoc) MarshalJSON() ([]byte, error) { return protojson.Marshal(w.Doc) }

// UnmarshalJSON reads a protojson doc.
func (w *wireDoc) UnmarshalJSON(b []byte) error {
	w.Doc = &pb.Doc{}
	return unmarshalPB(b, w.Doc)
}

// wireModReq is a pb.ModifyRequest on the wire.
type wireModReq struct{ *pb.ModifyRequest }

// MarshalJSON renders the modify request as protojson.
func (w wireModReq) MarshalJSON() ([]byte, error) { return protojson.Marshal(w.ModifyRequest) }

// UnmarshalJSON reads a protojson modify request.
func (w *wireModReq) UnmarshalJSON(b []byte) error {
	w.ModifyRequest = &pb.ModifyRequest{}
	return unmarshalPB(b, w.ModifyRequest)
}

// wireDep is a pb.ModifyDep on the wire.
type wireDep struct{ *pb.ModifyDep }

// MarshalJSON renders the dependency entry as protojson.
func (w wireDep) MarshalJSON() ([]byte, error) { return protojson.Marshal(w.ModifyDep) }

// UnmarshalJSON reads a protojson dependency entry.
func (w *wireDep) UnmarshalJSON(b []byte) error {
	w.ModifyDep = &pb.ModifyDep{}
	return unmarshalPB(b, w.ModifyDep)
}

// unmarshalPB decodes protojson into m, wrapping the message type into the error
// so a malformed payload names what failed to parse.
func unmarshalPB(b []byte, m proto.Message) error {
	if err := protojson.Unmarshal(b, m); err != nil {
		return fmt.Errorf("unmarshal %T: %w", m, err)
	}
	return nil
}

// takeDocsMsg asks the client which docs the claimed task needs. The doc set is
// a function of the task, so it cannot be static config; it has to be a
// callback. Sent only when the client registered takeDocs.
type takeDocsMsg struct {
	Type string   `json:"type"`
	Task wireTask `json:"task"`
}

// docsMsg is the client's reply to takeDocs: the docs to claim, each identified
// by namespace and key. The gateway claims them (sorted by namespace,key to
// avoid dining-philosopher livelock) before sending doWork. A doc claim names
// only what the worker knows -- the namespace and key; the claimant and lease are
// the gateway's to set -- so it is a plain pair, not a pb.DocClaim.
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
// work. Task is the identical task a native DoWork would receive, as protojson,
// so a wire worker sees exactly what an in-process one does, down to fields like
// attempt a reaper or authorizer might use.
type doWorkMsg struct {
	Type string    `json:"type"`
	Task wireTask  `json:"task"`
	Docs []wireDoc `json:"docs,omitempty"`
}

// disposition is a worker's choice of what happens to the task, shared by every
// message where a worker makes that choice (result and ack). The fields are the
// exact knobs the Go worker's Retry/Move sentinels carry, so a wire worker
// selects a disposition with the same vocabulary a native one does. See
// sentinel for the mapping onto worker errors.
type disposition struct {
	Outcome string `json:"outcome"`
	Message string `json:"message,omitempty"` // retry/move/fatal detail
	After   string `json:"after,omitempty"`   // retry: delay before re-arrival, e.g. "30s"
	OrMove  string `json:"orMove,omitempty"`  // retry: quarantine queue once attempts exhaust
	To      string `json:"to,omitempty"`      // move: destination queue
}

// sentinel maps a disposition onto the worker sentinel error it names. A "ok" or
// empty outcome is not an error, so it returns (nil, false); every other outcome
// returns (err, true) with the sentinel to hand back to the worker. A malformed
// retry duration or an unknown outcome is a client protocol bug, so it maps to a
// FatalError that stops the worker rather than looping on the same bad message.
func (d disposition) sentinel() (error, bool) {
	switch d.Outcome {
	case outcomeOK, "":
		return nil, false
	case outcomeRetry:
		re := worker.RetryErrorf("%s", orDefault(d.Message, "worker requested retry"))
		if d.After != "" {
			after, err := time.ParseDuration(d.After)
			if err != nil {
				return worker.FatalErrorf("bad retry 'after' %q: %v", d.After, err), true
			}
			re = re.After(after)
		}
		if d.OrMove != "" {
			re = re.OrMoveTo(d.OrMove)
		}
		return re, true
	case outcomeMove:
		me := worker.MoveErrorf("%s", orDefault(d.Message, "worker requested move"))
		if d.To != "" {
			me = me.To(d.To)
		}
		return me, true
	case outcomeFatal:
		return worker.FatalErrorf("%s", orDefault(d.Message, "worker requested fatal")), true
	default:
		return worker.FatalErrorf("unknown outcome %q", d.Outcome), true
	}
}

// result is the client's reply to doWork: exactly one disposition. On "ok" the
// gateway commits Modification; an absent modification commits nothing and leaves
// the task to be reclaimed after its lease, faithful to a Go DoModify that
// returns no mods. "retry"/"move"/"fatal" carry the same knobs the Go sentinels
// do (see disposition).
//
// Ack is the shorthand for the overwhelmingly common "I consumed this task" case:
// with it set, the gateway also deletes the claimed task, so the worker does not
// echo the task's id/version/queue back just to point at the task it was handed
// (the gateway is the authoritative holder of the claimed task, so it deletes
// from its own copy at the stable version). Ack composes with other
// modifications, but if the modification already touches the claimed task (a
// change, delete, or depend on that id) the modification wins and the ack is
// suppressed -- an explicit disposition always beats the shorthand.
type result struct {
	Type string `json:"type"`
	disposition
	Ack          bool        `json:"ack,omitempty"`
	Modification *wireModReq `json:"modification,omitempty"`
}

// dependencyMsg tells the client the commit failed a dependency check and names
// exactly which task and doc dependencies failed, as the same pb.ModifyDep list
// the gRPC service attaches to a NOT_FOUND status. A leading DETAIL entry carries
// the human-readable message. The worker inspects the list to tell "my task was
// scooped" from "some other dependency failed" (the wire analog of
// entroq.DependencyError.Implicates) and replies with an ack.
type dependencyMsg struct {
	Type string    `json:"type"`
	Deps []wireDep `json:"deps"`
}

// successMsg tells the client the commit succeeded and it may run its post-commit
// step. It carries no payload: the client holds whatever state it computed during
// work in its own process, across the round trip.
type successMsg struct {
	Type string `json:"type"`
}

// done is the client's reply to a post-commit phase (success or dependency),
// carrying the disposition to apply to the task. The two phases interpret it
// differently, matching the Go worker exactly:
//
//   - After success the task has already committed, so only "ok" (continue) and
//     "fatal" (stop the worker) are meaningful; "retry"/"move" are no-ops that
//     the worker's OnSuccess layer logs and ignores (nothing left to retry or
//     move).
//   - After a dependency failure the full vocabulary applies, optimistically:
//     "ok" leaves the task to be reclaimed on lease expiry, and a retry/move/
//     fatal sentinel lands only if the task itself was not implicated (and so is
//     still validly claimed).
type done struct {
	Type string `json:"type"`
	disposition
}
