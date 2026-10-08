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

	"golang.org/x/sync/errgroup"

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
// One Gateway is one conn is one worker Run. The send and recv channels pair an
// instruction with the phase waiting to answer it, which holds only while a
// single task is in flight, so a second Run here would cross-wire replies
// between tasks. Concurrency comes from more sessions, each with its own
// Gateway and its own claimant.
type Gateway struct {
	conn            conn
	protocolVersion int32
	sessionID       string
	claimant        string
	worker          *worker.Worker[json.RawMessage]
	client          entroq.Client

	config       *Config
	workerSendCh chan *SendMessage
	workerRecvCh chan *RecvMessage
	cancel       context.CancelFunc
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

		workerSendCh: make(chan *SendMessage),
		workerRecvCh: make(chan *RecvMessage),
	}
	var versionErr error
	if version == 0 {
		versionErr = fmt.Errorf("versions %v not supported by server, which speaks %v", conf.ProtocolVersions, supportedVersions)
	}

	if versionErr == nil {
		opts := []worker.Option[json.RawMessage]{
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

// Handle does a single turn of Recv+Send. It handles all kinds of messages except configs.
func (g *Gateway) Handle(ctx context.Context) error {
	msg, err := g.conn.Recv(ctx)
	if err != nil {
		return fmt.Errorf("handle recv: %w", err)
	}

	// TODO: this is a state machine - make sure we have the expected type *or* a read request.
	switch msg.Type {
	case RecvQuit:
		// TODO: structured error.
		return fmt.Errorf("quit received")
	case RecvReady:
		ctx, cancel := context.WithCancel(ctx)
		g.cancel = cancel
		defer cancel()
		if err := g.runLoop(ctx); err != nil {
			return fmt.Errorf("worker loop: %w", err)
		}
		return nil
	}
	return nil
}

// runLoop runs the worker loop.
func (g *Gateway) runLoop(ctx context.Context) error {
	// Start a worker. We already received "ready" from the client.
	grp, ctx := errgroup.WithContext(ctx)

	// TODO: figure out lifetimes
	grp.Go(func() error {
		if err := g.worker.Run(ctx,
			worker.AsClaimant(g.claimant),
			worker.Watching(g.config.Queues...),
			worker.WithMaxAttempts(g.config.MaxAttempts),
			worker.WithMaxClaims(g.config.MaxClaims),
		); err != nil {
			return fmt.Errorf("worker run: %w", err)
		}
		return nil
	})

	// Now listen on the worker channel.
	// TODO: which context to use here?
	grp.Go(func() error {
		for {
			// First send.
			var sMsg *SendMessage
			select {
			case <-ctx.Done():
				return fmt.Errorf("gateway send canceled (%v): %w", context.Cause(ctx), ctx.Err())
			case sMsg = <-g.workerSendCh:
				if err := g.conn.Send(ctx, sMsg); err != nil {
					return fmt.Errorf("gateway hander send: %w", err)
				}
			}

			// Then receive, possibly multiple times if they're all read requests.
			if err := g.handleRecv(ctx, sMsg); err != nil {
				return fmt.Errorf("gateway handle recv: %w", err)
			}
		}
	})

	if err := grp.Wait(); err != nil {
		return fmt.Errorf("gateway run error: %w", err)
	}
	return nil
}

// handleRecv figures out what was wanted and attempts to honor it, checking expected against the sent type if not a read.
func (g *Gateway) handleRecv(ctx context.Context, sent *SendMessage) error {
	for {
		rMsg, err := g.conn.Recv(ctx)
		if err != nil {
			return fmt.Errorf("gateway handle recv: %w", err)
		}
		switch rMsg.Type {
		// If it's just a read, go ahead and handle it.
		// Note that this leaves us needing to receive again, since the read
		// handler also sends a reply.
		case RecvDocQuery, RecvTaskQuery, RecvNamespaceQuery, RecvQueueQuery, RecvTime:
			if err := g.handleReadAndRespond(ctx, rMsg); err != nil {
				return fmt.Errorf("handle read: %w", err)
			}
			continue
		case RecvQuit:
			g.cancel()
			return fmt.Errorf("asked to quit")
		default:
			if rMsg.Type != sent.Expect {
				return fmt.Errorf("handle recv expected %v, got %v", sent.Expect, rMsg.Type)
			}
		}

		// The phase waiting on this answer gets it, and the turn is over: the
		// next receive waits for the next instruction to go out.
		select {
		case <-ctx.Done():
			return fmt.Errorf("gateway send canceled (%v): %w", context.Cause(ctx), ctx.Err())
		case g.workerRecvCh <- rMsg:
			return nil
		}
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
	return func(ctx context.Context, reader entroq.Reader, w *worker.Work[json.RawMessage]) (*worker.Result, error) {
		msg := g.newSend(SendWork, RecvModify)
		msg.Task = w.Task
		msg.DocSets = w.Sets

		select {
		case g.workerSendCh <- msg:
		case <-ctx.Done():
			return nil, fmt.Errorf("take docs send (%v): %w", context.Cause(ctx), ctx.Err())
		}

		select {
		case received := <-g.workerRecvCh:
			mod := worker.Modify(modifyingAll(received.Modification))
			if g.config.SendDependency {
				mod.OnDependency(g.MakeOnDependency())
			}
			if g.config.SendSuccess {
				mod.OnSuccess(g.MakeOnSuccess())
			}
			return mod, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("domodify recv canceled (%v): %w", context.Cause(ctx), ctx.Err())
		}
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
func (g *Gateway) MakeOnSuccess() func(context.Context) error {
	return func(ctx context.Context) error {
		msg := g.newSend(SendSuccess, RecvError)
		select {
		case g.workerSendCh <- msg:
		case <-ctx.Done():
			return fmt.Errorf("success recv (%v): %w", context.Cause(ctx), ctx.Err())
		}

		select {
		case received := <-g.workerRecvCh:
			if received.Err != "" {
				return errors.New(received.Err)
			}
			return nil
		case <-ctx.Done():
			return fmt.Errorf("recv dependency (%v): %w", context.Cause(ctx), ctx.Err())
		}
	}
}

// MakeOnDependency makes an OnDependency handler to be added to a modify result.
func (g *Gateway) MakeOnDependency() func(context.Context, *entroq.DependencyError) error {
	return func(ctx context.Context, depErr *entroq.DependencyError) error {
		msg := g.newSend(SendDependency, RecvError)
		msg.DepErr = depErr
		select {
		case g.workerSendCh <- msg:
		case <-ctx.Done():
			return fmt.Errorf("send dependency (%v): %w", context.Cause(ctx), ctx.Err())
		}

		select {
		case received := <-g.workerRecvCh:
			if received.Err != "" {
				return errors.New(received.Err)
			}
			return nil
		case <-ctx.Done():
			return fmt.Errorf("recv dependency (%v): %w", context.Cause(ctx), ctx.Err())
		}
	}
}

// MakeTakeDocs makes a TakeDocs handler for this gateway that can be passed into a Run.
//
// Only the sets a client names survive. The worker claims them as itself, until
// the task's own arrival, and discards any lease or claimant in the args, so
// there is nothing here for a client to say about how its docs are held.
func (g *Gateway) MakeTakeDocs() worker.TakeRun[json.RawMessage] {
	return func(ctx context.Context, reader entroq.Reader, w *worker.Work[json.RawMessage]) (*worker.TakeResult, error) {
		msg := g.newSend(SendDocs, RecvTake)
		msg.Task = w.Task
		select {
		case g.workerSendCh <- msg:
		case <-ctx.Done():
			return nil, fmt.Errorf("take docs send (%v): %w", context.Cause(ctx), ctx.Err())
		}

		select {
		case received := <-g.workerRecvCh:
			if received.Type != RecvTake {
				return nil, fmt.Errorf("invalid takedocs recv type %v", received.Type)
			}
			args := make([]entroq.DocClaimArg, 0, len(received.DocSetClaims))
			for _, c := range received.DocSetClaims {
				args = append(args, c)
			}
			return worker.Take(args...), nil
		case <-ctx.Done():
			return nil, fmt.Errorf("take docs recv (%v): %w", context.Cause(ctx), ctx.Err())
		}
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
