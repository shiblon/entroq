// Package workgateway implements the worker protocol in terms of over-the-wire
// implementations. Client writers who want to ensure that their EntroQ workers
// are fully compliant and as safe as possible should use this gateway instead
// of rolling their own subtle worker logic.
//
// The gateway can be accessed over HTTP/1 or over stdio pipes, depending on
// needs. The wire protocol is the same for any transport.
//
// This package is currently EXPERIMENTAL and can change at any time.
package workgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

// conn carries the turn-based protocol's JSON messages between the handler
// implementation and the gateway. Send and Recv are from the perspective of
// the gateway.
type conn interface {
	Send(context.Context, *SendMessage) error
	Recv(context.Context) (*RecvMessage, error)
}

// Config is a worker's registration, supplied by the transport out-of-band at
// connection time (flags/env for a spawned pipe gateway, URL params/headers for
// a WebSocket connection), never as a wire message. It is connection-scoped and
// fixed for the session. The lease is deliberately not here: it governs renewal
// cadence and reclaim latency, operational concerns owned by whoever runs the
// gateway, not by a connecting client.
type Config struct {
	// Queues tells the the worker what to listen on. At least one required.
	Queues []string `json:"queues"`

	// ErrorQueue names the queue a task moves to when it is quarantined or
	// moved with no destination, as a worker.ErrQTemplate: "{inbox}" stands
	// for the task's own queue, so "{inbox}/err" is the default.
	ErrorQueue string `json:"error_queue"`

	// MaxAttempts sets maximum attempts with RetryError before quarantining.
	// Unlimited when set to its zero value.
	MaxAttempts int32 `json:"max_attempts"`

	// MaxClaims sets maximum claims without modification before quarantine.
	// Unlimited when set to its zero value.
	MaxClaims int32 `json:"max_claims"`

	// SendDocs indicates that the client takes documents.
	SendDocs bool `json:"send_docs"`

	// SendSuccess indicates that the client accepts on-success events.
	SendSuccess bool `json:"send_success"`

	// SendDependency indicates that the client accepts on-dependency-error events.
	SendDependency bool `json:"send_dependency"`

	// ProtocolVersions lists the protocol versions that the client understands
	// and can react to. The server can reject the initial config message if it
	// does not support any of the listed gateway protocol numbers. Must not be
	// empty.
	ProtocolVersions []int32 `json:"protocol_versions"`

	// DangerousClaimantOverride overrides the auto-assigned gateway claimant.
	// Usually you should not do this, as it has serious implications for task
	// and doc ownership safety, and you need to establish your own guarantees.
	DangerousClaimantOverride string `json:"dangerous_claimant_override"`

	// RetryDelayS is the base delay before a retried task is available again,
	// in seconds. Leave at zero to use the default.
	RetryDelayS int32 `json:"retry_delay_s"`
}

// supportedVersions indicate the gateway protocol supported by this service.
var supportedVersions = []int32{1}

// SendType is the type of a message sent to the client.
type SendType string

// Send consts indiate the type of message the gateway is sending.
const (
	SendAck                SendType = "ack"
	SendDocs               SendType = "docs"
	SendWork               SendType = "work"
	SendSuccess            SendType = "success"
	SendDependency         SendType = "dependency"
	SendTime               SendType = "time"
	SendDocsResponse       SendType = "docs_response"
	SendTasksResponse      SendType = "tasks_response"
	SendQueuesResponse     SendType = "queues_response"
	SendNamespacesResponse SendType = "namespaces_response"
	SendQuit               SendType = "quit"
)

// SendMessage is the wire type for outbound messages from the gateway to the client.
type SendMessage struct {
	Version  int32  `json:"version"`
	Session  string `json:"session"`
	Claimant string `json:"claimant"`

	Type SendType `json:"type"`

	// Expect indicates the type of message should come back.
	// Mostly informational to help clients that might have the protocol wrong.
	Expect RecvType `json:"expect"`

	Task    *entroq.Task     `json:"task"`
	DocSets []*entroq.DocSet `json:"doc_sets"`

	DepErr *entroq.DependencyError `json:"dep_err"`

	// TimeMs answers a time read, in epoch millis, the way every other instant
	// in EntroQ is carried.
	TimeMs int64 `json:"time_ms"`

	// TODO: err needs to be structured
	Err string
}

// RecvType is the type of message received from the client.
type RecvType string

// Recv consts indicate the type of message the gateway has received.
const (
	RecvConfig         RecvType = "config"
	RecvReady          RecvType = "ready"
	RecvTake           RecvType = "take"
	RecvModify         RecvType = "modify"
	RecvTime           RecvType = "time"
	RecvTaskQuery      RecvType = "tasks"
	RecvDocQuery       RecvType = "docs"
	RecvQueueQuery     RecvType = "queues"
	RecvNamespaceQuery RecvType = "namespaces"
	RecvError          RecvType = "error"
	RecvQuit           RecvType = "quit"
)

// RecvMessage is the wire type for inbound messages from the client to the gateway.
type RecvMessage struct {
	Version int32  `json:"version"`
	Session string `json:"session"`

	// Type is the kind of message received.
	Type RecvType `json:"type"`

	DocSetClaims []*entroq.DocSetClaim `json:"doc_set_claims"`

	// Modification contains everything needed to modify tasks and docs.
	// Valid for type "modify", always passed when work is done.
	Modification *entroq.Modification `json:"modification"`

	// TODO: err needs to be structured.
	Err string
}

// Gateway handles all of the connections and protocol, passing data between
// the client handlers and the EntroQ worker.
//
// Protocol:
//
//	Client -> config        -> Gateway
//	Client <- reject/accept -> Gateway
//	Client -> Ready         -> Gateway
//	... (Gateway waits on a claim)
//	Client <- TakeDocs      <- Gateway
//	Client -> Docs Response -> Gateway
//	Client <- Task, Docs    <- Gateway
//	Client -> Modification  -> Gateway
//	... (Gateway waits on a claim)
//
//	TODO: we need to allow reading of data during work.
//
// One Gateway is one conn is one worker Run, and the phases talk to the conn
// directly. They can, because they are strictly sequential: one task is in
// flight at a time, so whichever phase is running is the only thing that could
// be speaking. A second Run here would break that and cross-wire replies
// between tasks. Concurrency comes from more sessions, each with its own
// Gateway and its own claimant.
type Gateway struct {
	conn            conn
	protocolVersion int32
	sessionID       string
	claimant        string
	worker          *worker.Worker[json.RawMessage]
	client          entroq.Client
	config          *Config

	// mu guards the session's cancellation, which Close reaches for and
	// Handle installs. Close can arrive first, from a transport that lost its
	// client before the worker ever started.
	mu     sync.Mutex
	cancel context.CancelFunc
	closed bool
}

// Start creates a new Gateway. An initial config request with a config should
// create a new gateway with its own state and session ID, and it sends an ack.
func Start(ctx context.Context, eq entroq.Client, conf *Config, conn conn) (*Gateway, error) {
	if len(conf.ProtocolVersions) == 0 {
		return nil, fmt.Errorf("no protocol versions requested in config: %v", conf)
	}

	version := highestOverlapping(conf.ProtocolVersions, supportedVersions)
	session := entroq.GenHex16()
	claimant := conf.DangerousClaimantOverride
	if claimant == "" {
		claimant = "workgateway/" + session
	}
	g := &Gateway{
		conn:            conn,
		config:          conf,
		protocolVersion: version,
		sessionID:       session,
		client:          eq,
		claimant:        claimant,
	}
	var versionErr error
	if version == 0 {
		versionErr = fmt.Errorf("versions %v not supported by server, which speaks %v", conf.ProtocolVersions, supportedVersions)
	}

	if versionErr == nil {
		// The cue sends the work; DoModify waits for the answer. Splitting
		// them is what keeps a client that never collects its task from
		// pinning it: see worker.Handler.CueWork.
		opts := []worker.Option[json.RawMessage]{
			worker.WithCueWork(g.MakeCueWork()),
			worker.WithDoModify(g.MakeDoModify()),
		}
		if conf.SendDocs {
			opts = append(opts, worker.WithTakeDocs(g.MakeTakeDocs()))
		}
		// TODO: ErrorQueue needs a worker.ErrQMap built from the template.
		g.worker = worker.New(eq, opts...)
	}

	sMsg := g.newSend(SendAck, RecvReady)
	if versionErr != nil {
		sMsg.Err = versionErr.Error()
	}
	sMsg.Claimant = g.claimant
	if err := g.conn.Send(ctx, sMsg); err != nil {
		return nil, fmt.Errorf("failed ack: %w", err)
	}

	// Version failure happens after sending the version error.
	if versionErr != nil {
		return nil, versionErr
	}

	return g, nil
}

// newSend makes a new SendMessage with standard parameters.
func (g *Gateway) newSend(typ SendType, expect RecvType) *SendMessage {
	return &SendMessage{
		Version: g.protocolVersion,
		Type:    typ,
		Expect:  expect,
		Session: g.sessionID,
	}
}

// Handle runs this session: it waits for the client to say it is ready, then
// runs the worker loop until the client stops, the worker stops, or Close.
//
// It blocks for the life of the session, so a transport runs it in a goroutine
// of its own -- and on a context of the transport's own lifetime, never one
// request's, which would end the session as soon as the reply to config had
// been written.
func (g *Gateway) Handle(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g.mu.Lock()
	closed := g.closed
	g.cancel = cancel
	g.mu.Unlock()
	if closed {
		return fmt.Errorf("session %s was closed before it ran", g.sessionID)
	}

	msg, err := g.conn.Recv(ctx)
	if err != nil {
		return fmt.Errorf("handle recv: %w", err)
	}
	switch msg.Type {
	case RecvQuit:
		// TODO: structured error.
		return fmt.Errorf("quit received")
	case RecvReady:
		if err := g.worker.Run(ctx,
			worker.AsClaimant(g.claimant),
			worker.Watching(g.config.Queues...),
			worker.WithMaxAttempts(g.config.MaxAttempts),
			worker.WithMaxClaims(g.config.MaxClaims),
		); err != nil {
			return fmt.Errorf("worker run: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("expected %q to begin the session, got %q", RecvReady, msg.Type)
	}
}

// Close ends the session from outside it: a transport whose client has gone, or
// a reaper that found the session idle. Safe to call more than once, and before
// Handle has started.
//
// Whatever the worker was holding goes back the way an abandoned claim always
// does: its lease lapses. Nothing is written on the way out.
func (g *Gateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	if g.cancel != nil {
		g.cancel()
	}
}

// recv waits for the client's answer to the instruction a phase just sent,
// answering any read requests that arrive first.
//
// Reads are interleaved WITHIN a turn rather than being turns of their own: a
// client may ask as many questions as it likes while it decides, and the turn
// ends only when the answer the phase is waiting for arrives.
func (g *Gateway) recv(ctx context.Context, expect RecvType) (*RecvMessage, error) {
	for {
		msg, err := g.conn.Recv(ctx)
		if err != nil {
			return nil, fmt.Errorf("gateway recv: %w", err)
		}
		switch msg.Type {
		case RecvDocQuery, RecvTaskQuery, RecvNamespaceQuery, RecvQueueQuery, RecvTime:
			if err := g.handleReadAndRespond(ctx, msg); err != nil {
				return nil, fmt.Errorf("handle read: %w", err)
			}
			continue
		case RecvQuit:
			// TODO: quit belongs on a work reply, so the result it carries can
			// commit before the session ends. As a message of its own it can
			// only arrive where an answer was due.
			return nil, fmt.Errorf("client asked to quit while %q was due", expect)
		}
		if msg.Type != expect {
			return nil, fmt.Errorf("gateway recv expected %v, got %v", expect, msg.Type)
		}
		return msg, nil
	}
}

func (g *Gateway) handleReadAndRespond(ctx context.Context, msg *RecvMessage) error {
	switch msg.Type {
	case RecvTime:
		t, err := g.client.Time(ctx)
		if err != nil {
			return fmt.Errorf("recv time: %w", err)
		}
		reply := g.newSend(SendTime, "")
		reply.TimeMs = t.UnixMilli()
		if err := g.conn.Send(ctx, reply); err != nil {
			return fmt.Errorf("send time: %w", err)
		}
		// TODO - other read cases, tasks, docs, queues, namespaces
	}
	return nil
}

// MakeDoModify makes a DoModify handler for this gateway that can be passed into a Run.
func (g *Gateway) MakeDoModify() worker.DoModifyRun[json.RawMessage] {
	return func(ctx context.Context, _ entroq.Reader, _ *worker.Work[json.RawMessage]) (*worker.Result, error) {
		// The cue already sent the work. This phase only collects the answer,
		// and it is the half that runs under renewal -- which is the point of
		// the split: the client has the task, so holding the claim open for it
		// is right. Waiting for a client that never collected it would not be.
		received, err := g.recv(ctx, RecvModify)
		if err != nil {
			return nil, fmt.Errorf("work: %w", err)
		}
		mod := worker.Modify(modifyingAll(received.Modification))
		if g.config.SendDependency {
			mod.OnDependency(g.MakeOnDependency())
		}
		if g.config.SendSuccess {
			mod.OnSuccess(g.MakeOnSuccess())
		}
		return mod, nil
	}
}

// MakeCueWork makes a CueWork handler for this gateway that can be passed into
// a Run: it hands the task and its held doc sets to the client.
//
// Only the send lives here, and that placement is the whole reason it is a
// phase of its own. Nothing renews the claim while a cue is in flight, so a
// client that never collects its task costs this session and frees the task
// for another worker. The same send from the work phase would renew the claim
// for as long as nobody collected it, and no other worker could ever have it.
func (g *Gateway) MakeCueWork() worker.CueWorkRun[json.RawMessage] {
	return func(ctx context.Context, _ entroq.Reader, w *worker.Work[json.RawMessage]) error {
		msg := g.newSend(SendWork, RecvModify)
		msg.Task = w.Task
		msg.DocSets = w.Sets
		if err := g.conn.Send(ctx, msg); err != nil {
			return fmt.Errorf("cue work: %w", err)
		}
		return nil
	}
}

// modifyingAll adds everything the client asked for to the worker's
// modification. A nil modification adds nothing, which commits the task's own
// disposition alone.
//
// The client's Claimant is deliberately not carried over: the claimant belongs
// to the run, whose client appends its own last so that it wins regardless.
func modifyingAll(src *entroq.Modification) entroq.ModifyArg {
	return func(m *entroq.Modification) {
		if src == nil {
			return
		}
		m.Inserts = append(m.Inserts, src.Inserts...)
		m.Changes = append(m.Changes, src.Changes...)
		m.Deletes = append(m.Deletes, src.Deletes...)
		m.Depends = append(m.Depends, src.Depends...)
		m.Arrives = append(m.Arrives, src.Arrives...)
		m.DocInserts = append(m.DocInserts, src.DocInserts...)
		m.DocChanges = append(m.DocChanges, src.DocChanges...)
		m.DocDeletes = append(m.DocDeletes, src.DocDeletes...)
		m.DocDepends = append(m.DocDepends, src.DocDepends...)
		m.DocArrives = append(m.DocArrives, src.DocArrives...)
	}
}

// MakeOnSuccess makes an OnSuccess handler to be added to a modify result.
//
// The post-commit hooks send and receive in one phase, which costs nothing:
// the commit has landed and renewal has stopped, so there is no claim left for
// a slow client to hold open.
func (g *Gateway) MakeOnSuccess() func(context.Context) error {
	return func(ctx context.Context) error {
		if err := g.conn.Send(ctx, g.newSend(SendSuccess, RecvError)); err != nil {
			return fmt.Errorf("send success: %w", err)
		}
		received, err := g.recv(ctx, RecvError)
		if err != nil {
			return fmt.Errorf("success: %w", err)
		}
		if received.Err != "" {
			return errors.New(received.Err)
		}
		return nil
	}
}

// MakeOnDependency makes an OnDependency handler to be added to a modify result.
func (g *Gateway) MakeOnDependency() func(context.Context, *entroq.DependencyError) error {
	return func(ctx context.Context, depErr *entroq.DependencyError) error {
		msg := g.newSend(SendDependency, RecvError)
		msg.DepErr = depErr
		if err := g.conn.Send(ctx, msg); err != nil {
			return fmt.Errorf("send dependency: %w", err)
		}
		received, err := g.recv(ctx, RecvError)
		if err != nil {
			return fmt.Errorf("dependency: %w", err)
		}
		if received.Err != "" {
			return errors.New(received.Err)
		}
		return nil
	}
}

// MakeTakeDocs makes a TakeDocs handler for this gateway that can be passed into a Run.
//
// Only the sets a client names survive. The worker claims them as itself, until
// the task's own arrival, and discards any lease or claimant in the args, so
// there is nothing here for a client to say about how its docs are held.
func (g *Gateway) MakeTakeDocs() worker.TakeRun[json.RawMessage] {
	return func(ctx context.Context, _ entroq.Reader, w *worker.Work[json.RawMessage]) (*worker.TakeResult, error) {
		msg := g.newSend(SendDocs, RecvTake)
		msg.Task = w.Task
		if err := g.conn.Send(ctx, msg); err != nil {
			return nil, fmt.Errorf("send take docs: %w", err)
		}
		received, err := g.recv(ctx, RecvTake)
		if err != nil {
			return nil, fmt.Errorf("take docs: %w", err)
		}
		args := make([]entroq.DocClaimArg, 0, len(received.DocSetClaims))
		for _, c := range received.DocSetClaims {
			args = append(args, c)
		}
		return worker.Take(args...), nil
	}
}

// highestOverlapping returns the highest positive overlapping number in two
// slices. It discards negative values and returns 0 if there is no overlap.
func highestOverlapping(a, b []int32) int32 {
	have := make(map[int32]bool, len(a))
	for _, v := range a {
		if v > 0 {
			have[v] = true
		}
	}
	var best int32
	for _, v := range b {
		if v > best && have[v] {
			best = v
		}
	}
	return best
}
