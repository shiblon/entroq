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
	"log"
	"sync"
	"time"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/pbconv"
	"github.com/shiblon/entroq/pkg/version"
	"github.com/shiblon/entroq/pkg/worker"
)

// conn carries the turn-based protocol's JSON messages between the handler
// implementation and the gateway. Send and Recv are from the perspective of
// the gateway.
// A transport reports a client that has gone away in its OWN idiom rather than
// through anything this package invents: io.EOF when a pipe closes, a canceled
// context when an HTTP request dies. classify treats both as a clean stop,
// because hanging up is how a client says goodbye.
type Conn interface {
	Send(context.Context, *Response) error
	Recv(context.Context) (*Request, error)

	// Close ends the conversation, and means whatever ending it means for the
	// transport: an HTTP service drops this connection and keeps serving
	// everyone else, while a pipe gateway is its one session and exits.
	Close() error
}

// Config is a worker's registration, carried by the first message of a session.
// It is connection-scoped and fixed for the session.
//
// It is a field of a Request rather than a message shape of its own so that
// every transport has ONE thing to decode. An HTTP service reads a request
// body, finds the type and the session id, and knows what to do with it --
// without a separate registration path that only the first request takes.
//
// The lease is deliberately not here: there is no client knowledge in it, since
// how fast a lost task is recovered is a property of the deployment and not of
// the work. Start takes it as an option.
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

	// RespDocs indicates that the client takes documents.
	WantDocs bool `json:"want_docs"`

	// RespSuccess indicates that the client accepts on-success events.
	WantSuccess bool `json:"want_success"`

	// RespDependency indicates that the client accepts on-dependency-error events.
	WantDependency bool `json:"want_dependency"`

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

	// WorkTimeoutS is how long this client expects to need for one task, in
	// seconds, after which the gateway stops waiting and the task is retried
	// with its attempt counted. Zero takes the operator's bound.
	//
	// The client asks because the client is what knows how long its work takes.
	// The operator caps it with WithMaxWorkTimeout, because an unbounded wait
	// holds a task under renewal where no other worker can reach it. A clamped
	// request is reported back in the check, so a worker written against a
	// budget it did not get does not merely look flaky.
	WorkTimeoutS int32 `json:"work_timeout_s"`
}

// Option configures a gateway session at Start. These are the operator's
// settings, as against a client's Config.
type Option func(*gatewayOpts)

type gatewayOpts struct {
	lease          time.Duration
	maxWorkTimeout time.Duration
}

// WithLease sets how long a claim is held before it must be renewed, which is
// also how long a task waits to be reclaimed when a worker dies. Shorter
// recovers faster and renews more often.
func WithLease(d time.Duration) Option {
	return func(o *gatewayOpts) {
		o.lease = d
	}
}

// WithMaxWorkTimeout caps what a client may ask for in Config.WorkTimeoutS,
// and is what a client that asks for nothing gets.
//
// Some bound is what catches a client that took its work and then died:
// nothing on the wire reports that, because a client in the middle of work owes
// nothing but its answer, and that answer may legitimately be slow. Pass zero
// to let a client ask for whatever it likes, including no bound at all.
func WithMaxWorkTimeout(d time.Duration) Option {
	return func(o *gatewayOpts) {
		o.maxWorkTimeout = d
	}
}

// workTimeout is how long this session lets a client hold a task: what it
// asked for, capped by what the operator allows, and the operator's own bound
// when it asked for nothing.
//
// Zero means no bound, which only an operator can choose: a client's zero is
// "no preference", not "forever".
func workTimeout(want, ceiling time.Duration) time.Duration {
	if want <= 0 {
		return ceiling
	}
	if ceiling > 0 && want > ceiling {
		return ceiling
	}
	return want
}

// supportedVersions indicate the gateway protocol supported by this service.
var supportedVersions = []int32{1}

// ResponseType is the type of a message sent to the client.
type ResponseType string

// Send consts indicate the type of message the gateway is sending.
const (
	RespCheck      ResponseType = "check"
	RespTake       ResponseType = "take"
	RespWork       ResponseType = "work"
	RespSuccess    ResponseType = "success"
	RespDependency ResponseType = "dependency"
	RespTime       ResponseType = "time"
	RespDocs       ResponseType = "docs"
	RespTasks      ResponseType = "tasks"
	RespQueues     ResponseType = "queues"
	RespNamespaces ResponseType = "namespaces"
	RespQuit       ResponseType = "quit"

	// RespError answers a read that failed. It is the read failing, not the
	// session: the client hears why and decides, and the task in hand is
	// untouched.
	RespError ResponseType = "error"
)

// Response is the wire type for outbound messages from the gateway to the client.
type Response struct {
	Version  int32  `json:"version"`
	Session  string `json:"session"`
	Claimant string `json:"claimant"`

	// WorkTimeoutS, in a check, is how long this session will actually wait for
	// a client's answer: what it asked for, or less if the operator caps it
	// lower. Zero is no bound at all.
	WorkTimeoutS int32 `json:"work_timeout_s,omitempty"`

	Type ResponseType `json:"type"`

	// Expect indicates the type of message should come back.
	// Mostly informational to help clients that might have the protocol wrong.
	Expect RequestType `json:"expect"`

	Task    *wireTask `json:"task,omitempty"`
	DocSets []wireSet `json:"doc_sets,omitempty"`

	// Deps names the tasks and docs a commit depended on and did not find, so
	// a client can tell which of them moved rather than parse a message.
	Deps []wireDep `json:"deps,omitempty"`

	// Class and Message say why the gateway will not go on. Class is an
	// ExitClass token ("transient", "caller", "gateway"), so a client branches
	// on whether reconnecting could help rather than reading the message.
	//
	// This is the SESSION failing, not a task: a task's fate travels the other
	// way, as the Outcome a client reports.
	Class   string `json:"class,omitempty"`
	Message string `json:"message,omitempty"`

	// A read answers in exactly one of these, named by Type, and each carries
	// the content of the gRPC service response for the same read.
	Tasks      []wireTask          `json:"tasks,omitempty"`
	Docs       []wireDoc           `json:"docs,omitempty"`
	Queues     []wireQueueStats    `json:"queues,omitempty"`
	Namespaces []wireNamespaceStat `json:"namespaces,omitempty"`

	// TimeMs answers a time read, in epoch millis, the way every other instant
	// in EntroQ is carried.
	TimeMs int64 `json:"time_ms,omitempty"`
}

// RequestType is the type of message received from the client.
type RequestType string

// Recv consts indicate the type of message the gateway has received.
const (
	ReqConfig     RequestType = "config"
	ReqReady      RequestType = "ready"
	ReqTake       RequestType = "take"
	ReqModify     RequestType = "modify"
	ReqTime       RequestType = "time"
	ReqTasks      RequestType = "tasks"
	ReqDocs       RequestType = "docs"
	ReqQueues     RequestType = "queues"
	ReqNamespaces RequestType = "namespaces"
	ReqError      RequestType = "error"
	ReqQuit       RequestType = "quit"
)

// Request is the wire type for inbound messages from the client to the gateway.
type Request struct {
	Version int32  `json:"version"`
	Session string `json:"session"`

	// Type is the kind of message received.
	Type RequestType `json:"type"`

	// DocSetClaims names the doc sets this task needs. Valid for type "take".
	DocSetClaims []wireClaim `json:"doc_set_claims,omitempty"`

	// Outcome is the task's disposition, on a "modify" reply and on the
	// post-commit replies. Empty means OutcomeOK.
	Outcome Outcome `json:"outcome,omitempty"`

	// Error says why, for any outcome but ok. It is structured because the
	// outcomes it accompanies are decisions the worker acts on, and a client
	// should never have to encode one of those in prose.
	Error *TaskError `json:"error,omitempty"`

	// Config is the client's registration, and the first message of a session.
	// Valid for type "config", and ignored after that: a session is registered
	// once and does not change.
	Config *Config `json:"config,omitempty"`

	// A read request carries exactly one of these, named by Type. They are the
	// gRPC service request messages, so a client asks the question EntroQ
	// already answers rather than learning a query language of this protocol.
	TasksQuery *wireTasksReq `json:"tasks_query,omitempty"`
	DocsQuery  *wireDocsReq  `json:"docs_query,omitempty"`
	MatchQuery *wireMatchReq `json:"match_query,omitempty"`

	// Quit asks the gateway to finish this task and then stop claiming.
	//
	// It rides on a reply rather than arriving as a message of its own, so the
	// result it accompanies commits before the session ends. A quit with
	// nothing in hand would have nothing to drain.
	Quit bool `json:"quit,omitempty"`

	// Modification contains everything needed to modify tasks and docs.
	// Valid for type "modify", always passed when work is done.
	//
	// Leave claimant_id empty: the gateway owns the claim and attributes the
	// commit itself, and a modification that names a claimant is refused
	// rather than silently overridden. Arrivals are durations (by_ms), never
	// instants -- see pbconv.ModifyArgsFromProto, which validates this one.
	Modification *wireModReq `json:"modification,omitempty"`
}

// Gateway handles all of the connections and protocol, passing data between
// the client handlers and the EntroQ worker.
//
// Protocol:
//
//	Client -> Config   -> Gateway
//	Client <- Check	   -> Gateway
//	Client -> Ready    -> Gateway
//	... (Gateway waits on a claim)
//	Client <- TakeDocs      <- Gateway
//	Client -> Doc Refs -> Gateway
//	Client <- Do Work  <- Gateway
//	Client -> Modify   -> Gateway
//	... (Gateway waits on a claim)
//
// One Gateway is one conn is one worker Run, and the phases talk to the conn
// directly. They can, because they are strictly sequential: one task is in
// flight at a time, so whichever phase is running is the only thing that could
// be speaking. A second Run here would break that and cross-wire replies
// between tasks. Concurrency comes from more sessions, each with its own
// Gateway and its own claimant.
type Gateway struct {
	sync.Mutex

	conn            Conn
	protocolVersion int32
	sessionID       string
	claimant        string
	worker          *worker.Worker[json.RawMessage]
	client          entroq.Client
	config          *Config

	opts gatewayOpts

	// workTimeout is what this session settled on, which the check reports so
	// a clamped client knows the budget it actually has.
	workTimeout time.Duration

	cancel   context.CancelFunc
	closed   bool
	quitting bool
}

// Serve runs one client's session over conn, from its config message until
// whatever ends it, and blocks for the life of the session.
//
// A transport calls this in a goroutine of its own, on a context of the
// TRANSPORT's lifetime rather than one request's, and keeps only the conn and
// that context's cancel. It needs nothing else: a session id arrives in the
// check, which the transport must read in order to answer the config request
// anyway, and which therefore reaches it before the client can send anything
// further.
//
// A registration this gateway will not serve is ANSWERED rather than returned,
// so the error here is for whoever runs the gateway. A client learns from the
// message.
func Serve(ctx context.Context, eq entroq.Client, conn Conn, options ...Option) error {
	if eq == nil || conn == nil {
		return fmt.Errorf("work gateway needs a client and a conn")
	}
	defer conn.Close()

	first, err := conn.Recv(ctx)
	if err != nil {
		return fmt.Errorf("recv config: %w", err)
	}

	g, err := newGateway(eq, first, conn, options...)
	if err != nil {
		if sendErr := conn.Send(ctx, &Response{
			Type:    RespError,
			Class:   ExitCaller.String(),
			Message: err.Error(),
		}); sendErr != nil {
			return fmt.Errorf("send refusal (%v): %w", err, sendErr)
		}
		return fmt.Errorf("config refused: %w", err)
	}

	// The check answers the config request that got us here, and carries the
	// three things a client cannot work out for itself: which session it is,
	// which consumer holds its tasks, and how long it will actually be given
	// for one of them.
	//
	// Sending it here is safe because Serve is already the goroutine: the
	// caller is not waiting on this send, it is waiting to read it.
	check := g.newSend(RespCheck, ReqReady)
	check.Claimant = g.claimant
	check.WorkTimeoutS = int32(g.workTimeout / time.Second)
	if err := conn.Send(ctx, check); err != nil {
		return fmt.Errorf("send check: %w", err)
	}
	return g.Handle(ctx)
}

// newGateway builds a session from a registration, or says why it will not.
//
// Every error it returns is a client's fault, which is what lets Serve answer
// all of them the same way. They are also all decided HERE, at config time: a
// registration that cannot work should not wait for the worker loop to say so.
func newGateway(eq entroq.Client, first *Request, conn Conn, options ...Option) (*Gateway, error) {
	if first == nil {
		return nil, fmt.Errorf("no first message")
	}
	if first.Type != ReqConfig {
		return nil, fmt.Errorf("a session opens with %q, got %q", ReqConfig, first.Type)
	}
	conf := first.Config
	if conf == nil {
		return nil, fmt.Errorf("a %q message carries no config", ReqConfig)
	}
	if len(conf.ProtocolVersions) == 0 {
		return nil, fmt.Errorf("config names no protocol versions; this gateway speaks %v", supportedVersions)
	}
	if len(conf.Queues) == 0 {
		return nil, fmt.Errorf("config names no queues to work on")
	}
	speaking := highestOverlapping(conf.ProtocolVersions, supportedVersions)
	if speaking == 0 {
		return nil, fmt.Errorf("config asks for protocols %v; this gateway speaks %v",
			conf.ProtocolVersions, supportedVersions)
	}

	// The defaults are the worker's own, so an operator who sets nothing gets
	// what a native worker would have done.
	opts := gatewayOpts{
		lease:          entroq.DefaultClaimDuration,
		maxWorkTimeout: worker.DefaultWorkTimeout,
	}
	for _, o := range options {
		o(&opts)
	}

	session := entroq.GenHex16()
	claimant := conf.DangerousClaimantOverride
	if claimant == "" {
		claimant = "workgateway/" + session
	}
	g := &Gateway{
		conn:            conn,
		config:          conf,
		protocolVersion: speaking,
		sessionID:       session,
		client:          eq,
		claimant:        claimant,
		opts:            opts,
		workTimeout: workTimeout(
			time.Duration(conf.WorkTimeoutS)*time.Second, opts.maxWorkTimeout),
	}

	// The cue sends the work; DoModify waits for the answer. Splitting them is
	// what keeps a client that never collects its task from pinning it: see
	// worker.Handler.CueWork.
	wOpts := []worker.Option[json.RawMessage]{
		worker.WithCueWork(g.makeCueWork()),
		worker.WithDoModify(g.makeDoModify()),
		// An empty template is DefaultErrQMap, so there is nothing to branch
		// on: a client naming no error queue gets "{inbox}/err".
		worker.WithErrQMap[json.RawMessage](worker.ErrQTemplate(conf.ErrorQueue)),
	}
	if conf.WantDocs {
		wOpts = append(wOpts, worker.WithTakeDocs(g.makeTakeDocs()))
	}
	g.worker = worker.New(eq, wOpts...)
	return g, nil
}

// newSend makes a new Response with standard parameters.
func (g *Gateway) newSend(typ ResponseType, expect RequestType) *Response {
	return &Response{
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

	// Close stops a running session by cancelling this, so installing it and
	// reading whether we are already closed happen in ONE critical section --
	// the one Close itself takes. A Close that landed between two separate
	// steps would find no cancel to call and the session would run on.
	//
	// Without this the worker survives its own shutdown: Close shuts the conn,
	// the run loop goes around, and Claim blocks on an empty queue forever,
	// because Claim never touches a conn.
	g.Lock()
	closed := g.closed
	g.cancel = cancel
	g.Unlock()
	if closed {
		return fmt.Errorf("session closed, can't serve")
	}

	msg, err := g.conn.Recv(ctx)
	if err != nil {
		return g.exit(fmt.Errorf("handle recv: %w", err))
	}
	switch msg.Type {
	case ReqQuit:
		// A client that quits before asking for anything has nothing in hand
		// to drain, so this is simply goodbye.
		return nil
	case ReqReady:
		return g.exit(g.worker.Run(ctx,
			worker.AsClaimant(g.claimant),
			worker.Watching(g.config.Queues...),
			worker.WithMaxAttempts(g.config.MaxAttempts),
			worker.WithMaxClaims(g.config.MaxClaims),
			worker.WithLease(g.opts.lease),
			worker.WithWorkTimeout(g.workTimeout),
		))
	default:
		return g.exit(worker.FatalErrorf("expected %q to begin the session, got %q", ReqReady, msg.Type))
	}
}

// Closed indicates whether the gateway is closed already.
func (g *Gateway) Closed() bool {
	g.Lock()
	defer g.Unlock()
	return g.closed
}

// Close ends the session from outside it: a transport whose client has gone, or
// a reaper that found the session idle. Safe to call more than once, and before
// Handle has started.
//
// Whatever the worker was holding goes back the way an abandoned claim always
// does: its lease lapses. Nothing is written on the way out.
func (g *Gateway) Close() {
	g.Lock()
	defer g.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	if g.cancel != nil {
		g.cancel()
	}
	// Close is what a reaper calls, and what runs when something has already
	// gone wrong, so it must not be the thing that panics: a session refused
	// partway through Start never got a conn.
	if g.conn != nil {
		if err := g.conn.Close(); err != nil {
			log.Printf("work gateway: closing session %s: %v", g.sessionID, err)
		}
	}
}

// recv waits for the client's answer to the instruction a phase just sent,
// answering any read requests that arrive first.
//
// Reads are interleaved WITHIN a turn rather than being turns of their own: a
// client may ask as many questions as it likes while it decides, and the turn
// ends only when the answer the phase is waiting for arrives.
func (g *Gateway) recv(ctx context.Context, expect RequestType) (*Request, error) {
	for {
		msg, err := g.conn.Recv(ctx)
		if err != nil {
			return nil, fmt.Errorf("gateway recv: %w", err)
		}
		switch msg.Type {
		case ReqDocs, ReqTasks, ReqNamespaces, ReqQueues, ReqTime:
			if err := g.handleReadAndRespond(ctx, msg); err != nil {
				return nil, fmt.Errorf("handle read: %w", err)
			}
			continue
		case ReqQuit:
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

// makeDoModify makes a DoModify handler for this gateway that can be passed into a Run.
func (g *Gateway) makeDoModify() worker.DoModifyRun[json.RawMessage] {
	return func(ctx context.Context, _ entroq.Reader, _ *worker.Work[json.RawMessage]) (*worker.Result, error) {
		// The cue already sent the work. This phase only collects the answer,
		// and it is the half that runs under renewal -- which is the point of
		// the split: the client has the task, so holding the claim open for it
		// is right. Waiting for a client that never collected it would not be.
		received, err := g.recv(ctx, ReqModify)
		if err != nil {
			return nil, fmt.Errorf("work: %w", err)
		}
		// A disposition other than ok replaces the commit: the worker retries,
		// moves or stops, and there is nothing to apply.
		if d := g.disposition(received.Outcome, received.Error); d != nil {
			return nil, d
		}
		args, err := g.modifyArgs(received.Modification)
		if err != nil {
			// A modification the gateway cannot make sense of is the client's
			// fault and no retry will fix it, so the worker stops rather than
			// claiming the task again to fail the same way.
			return nil, worker.FatalErrorf("work modification: %v", err)
		}
		if received.Quit {
			g.quit()
		}
		// BOTH hooks are always registered, whatever the client asked for.
		// They cost a wire trip only if it wants one, and the gateway needs
		// them either way: between them they cover both outcomes of a commit,
		// which is the only place a drain can act without losing anything.
		return worker.Modify(args...).
			OnSuccess(g.makeOnSuccess()).
			OnDependency(g.makeOnDependency()), nil
	}
}

// quit records that the client asked to stop. The session ends from a
// post-commit hook rather than here, so whatever is in hand commits first.
func (g *Gateway) quit() {
	g.Lock()
	defer g.Unlock()
	g.quitting = true
}

// quitRequested reports whether a client has asked to stop.
func (g *Gateway) quitRequested() bool {
	g.Lock()
	defer g.Unlock()
	return g.quitting
}

// modifyArgs converts the modification a client asked for, through the same
// validator the gRPC service uses: it checks what a modification must name,
// refuses a namespace move, and resolves arrivals the one way protocol 2
// allows -- as durations, which the server resolves on its own clock.
//
// A nil modification is no modification, which commits the task's own
// disposition alone.
func (g *Gateway) modifyArgs(mod *wireModReq) ([]entroq.ModifyArg, error) {
	if mod == nil || mod.ModifyRequest == nil {
		return nil, nil
	}
	// The claimant belongs to the run. The run's client appends its own last
	// and would win anyway, so this refusal is not protecting the commit -- it
	// is telling a client that believes otherwise that it is wrong.
	if id := mod.GetClaimantId(); id != "" {
		return nil, fmt.Errorf("modification names claimant %q: the gateway owns the claim and attributes the commit itself", id)
	}
	if err := namesQueues(mod.ModifyRequest); err != nil {
		return nil, err
	}
	args, err := pbconv.ModifyArgsFromProto(mod.ModifyRequest, version.Protocol)
	if err != nil {
		return nil, err
	}
	return args, nil
}

// namesQueues refuses a modification that names a task without naming its
// queue, which a wire client leaves out constantly: the task it was handed
// carries the queue, but nothing about a TaskID suggests it is required.
//
// The queue is not bookkeeping. Authorization is decided per queue, so a client
// that omits it is asking to be authorized against nothing, and the gateway
// will not supply one on its behalf -- inventing a queue is inventing an
// authorization decision, even where the task in hand would make the guess
// obvious.
//
// Backends do reject it, but as a dependency failure that reads like a WRONG
// queue rather than a missing one, which sends a client hunting a version race
// that is not there. This check belongs HERE rather than in pbconv: a backend
// treats the omission as a dependency failure, every door onto EntroQ must
// agree about that (see eqtest.ModifyRejectsWrongQueue), and only this gateway
// is free to be more helpful to its own clients.
func namesQueues(req *pb.ModifyRequest) error {
	for i, ins := range req.GetInserts() {
		if ins.GetQueue() == "" {
			return fmt.Errorf("insert %d names no queue", i)
		}
	}
	for i, chg := range req.GetChanges() {
		if chg.GetOldId().GetQueue() == "" {
			return fmt.Errorf("change %d of task %q names no queue", i, chg.GetOldId().GetId())
		}
	}
	for i, del := range req.GetDeletes() {
		if del.GetQueue() == "" {
			return fmt.Errorf("delete %d of task %q names no queue", i, del.GetId())
		}
	}
	for i, dep := range req.GetDepends() {
		if dep.GetQueue() == "" {
			return fmt.Errorf("depend %d on task %q names no queue", i, dep.GetId())
		}
	}
	return nil
}

// makeCueWork makes a CueWork handler for this gateway that can be passed into
// a Run: it hands the task and its held doc sets to the client.
//
// Only the send lives here, and that placement is the whole reason it is a
// phase of its own. Nothing renews the claim while a cue is in flight, so a
// client that never collects its task costs this session and frees the task
// for another worker. The same send from the work phase would renew the claim
// for as long as nobody collected it, and no other worker could ever have it.
func (g *Gateway) makeCueWork() worker.CueWorkRun[json.RawMessage] {
	return func(ctx context.Context, _ entroq.Reader, w *worker.Work[json.RawMessage]) error {
		task, err := taskToWire(w.Task)
		if err != nil {
			return worker.MoveErrorf("cue work: %v", err)
		}
		sets, err := setsToWire(w.Sets)
		if err != nil {
			return worker.MoveErrorf("cue work: %v", err)
		}
		msg := g.newSend(RespWork, ReqModify)
		msg.Task = task
		msg.DocSets = sets
		if err := g.conn.Send(ctx, msg); err != nil {
			return fmt.Errorf("cue work: %w", err)
		}
		return nil
	}
}

// makeOnSuccess is the hook the worker runs after a commit lands. It tells the
// client, if the client asked to be told, and ends the session if a quit has
// been requested.
//
// Closing here is safe in a way it would not be during work: the commit has
// already landed, so there is nothing in flight to lose.
func (g *Gateway) makeOnSuccess() func(context.Context) error {
	return func(ctx context.Context) error {
		var reported error
		if g.config.WantSuccess {
			reported = g.report(ctx, g.newSend(RespSuccess, ReqError))
		}
		if g.quitRequested() {
			return errors.Join(reported, g.drained(ctx))
		}
		return reported
	}
}

// makeOnDependency is the hook the worker runs when a commit loses a dependency
// race. The outcome the client reports picks the task fate, with the same
// vocabulary the work phase uses -- optimistically, since the commit already
// failed and the disposition lands only if this task was not itself what went
// missing.
//
// A quit ends the session here too, and the disposition is given up to do it:
// closing cancels the context the worker would write it on, so the task is
// left to its lease instead, which is this hook default anyway. Nothing is
// lost that was not already lost with the commit.
func (g *Gateway) makeOnDependency() func(context.Context, *entroq.DependencyError) error {
	return func(ctx context.Context, depErr *entroq.DependencyError) error {
		var reported error
		if g.config.WantDependency {
			msg := g.newSend(RespDependency, ReqError)
			// Deps leads with a DETAIL entry carrying the message and then
			// names each dependency that failed, so there is nothing a Message
			// would add that a client is not better off reading from the list.
			msg.Deps = depsToWire(depErr)
			reported = g.report(ctx, msg)
		}
		if g.quitRequested() {
			// The disposition is given up to stop: closing cancels the context
			// the worker would write it on, so the task is left to its lease,
			// which is this hook's default anyway.
			return g.drained(ctx)
		}
		return reported
	}
}

// drained answers the client's quit and then ends the session.
//
// The answer matters because of what a transport needs: EVERY request gets
// exactly one response. A quit arrives on a reply, and if the session simply
// stopped there would be no answer to that reply at all -- which over HTTP is a
// request that never completes, and over a pipe a client waiting for something
// that is not coming.
//
// It goes out before Close, because Close is what makes sending impossible.
func (g *Gateway) drained(ctx context.Context) error {
	sendErr := g.conn.Send(ctx, g.newSend(RespQuit, ""))
	g.Close()
	if sendErr != nil {
		return fmt.Errorf("send drained: %w", sendErr)
	}
	return nil
}

// report sends a post-commit message and turns the client answer into the
// disposition the hook returns. Both hooks do the same thing with it, so they
// say it once.
func (g *Gateway) report(ctx context.Context, msg *Response) error {
	if err := g.conn.Send(ctx, msg); err != nil {
		return fmt.Errorf("send %s: %w", msg.Type, err)
	}
	received, err := g.recv(ctx, ReqError)
	if err != nil {
		return fmt.Errorf("%s: %w", msg.Type, err)
	}
	if received.Quit {
		g.quit()
	}
	return g.disposition(received.Outcome, received.Error)
}

// makeTakeDocs makes a TakeDocs handler for this gateway that can be passed into a Run.
//
// Only the sets a client names survive. The worker claims them as itself, until
// the task's own arrival, and discards any lease or claimant in the args, so
// there is nothing here for a client to say about how its docs are held.
func (g *Gateway) makeTakeDocs() worker.TakeRun[json.RawMessage] {
	return func(ctx context.Context, _ entroq.Reader, w *worker.Work[json.RawMessage]) (*worker.TakeResult, error) {
		task, err := taskToWire(w.Task)
		if err != nil {
			return nil, worker.MoveErrorf("take docs: %v", err)
		}
		msg := g.newSend(RespTake, ReqTake)
		msg.Task = task
		if err := g.conn.Send(ctx, msg); err != nil {
			return nil, fmt.Errorf("send take docs: %w", err)
		}
		received, err := g.recv(ctx, ReqTake)
		if err != nil {
			return nil, fmt.Errorf("take docs: %w", err)
		}
		args, err := claimsFromWire(received.DocSetClaims)
		if err != nil {
			return nil, worker.FatalErrorf("take docs: %v", err)
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
