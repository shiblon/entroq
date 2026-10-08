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
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/shiblon/entroq"
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
type conn interface {
	Send(context.Context, *SendMessage) error
	Recv(context.Context) (*RecvMessage, error)

	// Close ends the conversation, and means whatever ending it means for the
	// transport: an HTTP service drops this connection and keeps serving
	// everyone else, while a pipe gateway is its one session and exits.
	Close() error
}

// Config is a worker's registration, supplied by the transport out-of-band at
// connection time (flags/env for a spawned pipe gateway, URL params/headers for
// a WebSocket connection), never as a wire message. It is connection-scoped and
// fixed for the session.
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

// SendType is the type of a message sent to the client.
type SendType string

// Send consts indicate the type of message the gateway is sending.
const (
	SendCheck      SendType = "check"
	SendTake       SendType = "take"
	SendWork       SendType = "work"
	SendSuccess    SendType = "success"
	SendDependency SendType = "dependency"
	SendTime       SendType = "time"
	SendDocs       SendType = "docs"
	SendTasks      SendType = "tasks"
	SendQueues     SendType = "queues"
	SendNamespaces SendType = "namespaces"
	SendQuit       SendType = "quit"

	// SendError answers a read that failed. It is the read failing, not the
	// session: the client hears why and decides, and the task in hand is
	// untouched.
	SendError SendType = "error"
)

// SendMessage is the wire type for outbound messages from the gateway to the client.
type SendMessage struct {
	Version  int32  `json:"version"`
	Session  string `json:"session"`
	Claimant string `json:"claimant"`

	// WorkTimeoutS, in a check, is how long this session will actually wait for
	// a client's answer: what it asked for, or less if the operator caps it
	// lower. Zero is no bound at all.
	WorkTimeoutS int32 `json:"work_timeout_s,omitempty"`

	Type SendType `json:"type"`

	// Expect indicates the type of message should come back.
	// Mostly informational to help clients that might have the protocol wrong.
	Expect RecvType `json:"expect"`

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

// RecvType is the type of message received from the client.
type RecvType string

// Recv consts indicate the type of message the gateway has received.
const (
	RecvConfig     RecvType = "config"
	RecvReady      RecvType = "ready"
	RecvTake       RecvType = "take"
	RecvModify     RecvType = "modify"
	RecvTime       RecvType = "time"
	RecvTasks      RecvType = "tasks"
	RecvDocs       RecvType = "docs"
	RecvQueues     RecvType = "queues"
	RecvNamespaces RecvType = "namespaces"
	RecvError      RecvType = "error"
	RecvQuit       RecvType = "quit"
)

// RecvMessage is the wire type for inbound messages from the client to the gateway.
type RecvMessage struct {
	Version int32  `json:"version"`
	Session string `json:"session"`

	// Type is the kind of message received.
	Type RecvType `json:"type"`

	// DocSetClaims names the doc sets this task needs. Valid for type "take".
	DocSetClaims []wireClaim `json:"doc_set_claims,omitempty"`

	// Outcome is the task's disposition, on a "modify" reply and on the
	// post-commit replies. Empty means OutcomeOK.
	Outcome Outcome `json:"outcome,omitempty"`

	// Error says why, for any outcome but ok. It is structured because the
	// outcomes it accompanies are decisions the worker acts on, and a client
	// should never have to encode one of those in prose.
	Error *TaskError `json:"error,omitempty"`

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

	conn            conn
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

// Start creates a Gateway for one client's session -- its own state, session ID
// and claimant -- and the check that answers the config request.
//
// A refusal is a message rather than an error, so a nil Gateway with a non-nil
// message means the config was refused and the message says why.
func Start(ctx context.Context, eq entroq.Client, conf *Config, conn conn, options ...Option) (*Gateway, *SendMessage, error) {
	if eq == nil || conn == nil || conf == nil {
		return nil, nil, fmt.Errorf("work gateway needs a client, a config and a conn")
	}
	// Everything a client got wrong is answered rather than returned, and
	// answered at config time: a registration that cannot work should not wait
	// until the worker loop to say so.
	if len(conf.ProtocolVersions) == 0 {
		return nil, refusal("config names no protocol versions; this gateway speaks %v", supportedVersions), nil
	}
	if len(conf.Queues) == 0 {
		return nil, refusal("config names no queues to work on"), nil
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

	speaking := highestOverlapping(conf.ProtocolVersions, supportedVersions)
	if speaking == 0 {
		return nil, refusal("config asks for protocols %v; this gateway speaks %v",
			conf.ProtocolVersions, supportedVersions), nil
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
	if conf.SendDocs {
		wOpts = append(wOpts, worker.WithTakeDocs(g.makeTakeDocs()))
	}
	g.worker = worker.New(eq, wOpts...)

	// Return the check instead of sending it: Start is blocking the caller,
	// and this would block Start if it were sent before the other side starts
	// listening.
	sMsg := g.newSend(SendCheck, RecvReady)
	sMsg.Claimant = g.claimant
	sMsg.WorkTimeoutS = int32(g.workTimeout / time.Second)
	return g, sMsg, nil
}

// refusal is the answer to a registration this gateway will not serve. It
// carries no session, because there is none, and the caller's fault class,
// because retrying the same config would fail the same way.
func refusal(format string, args ...any) *SendMessage {
	return &SendMessage{
		Type:    SendError,
		Class:   ExitCaller.String(),
		Message: fmt.Sprintf(format, args...),
	}
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
	g.Lock()
	closed := g.closed
	g.cancel = cancel
	g.Unlock()
	if closed {
		return fmt.Errorf("session %s was closed before it ran", g.sessionID)
	}

	msg, err := g.conn.Recv(ctx)
	if err != nil {
		return g.exit(fmt.Errorf("handle recv: %w", err))
	}
	switch msg.Type {
	case RecvQuit:
		// A client that quits before asking for anything has nothing in hand
		// to drain, so this is simply goodbye.
		return nil
	case RecvReady:
		return g.exit(g.worker.Run(ctx,
			worker.AsClaimant(g.claimant),
			worker.Watching(g.config.Queues...),
			worker.WithMaxAttempts(g.config.MaxAttempts),
			worker.WithMaxClaims(g.config.MaxClaims),
			worker.WithLease(g.opts.lease),
			worker.WithWorkTimeout(g.workTimeout),
		))
	default:
		return g.exit(worker.FatalErrorf("expected %q to begin the session, got %q", RecvReady, msg.Type))
	}
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
func (g *Gateway) recv(ctx context.Context, expect RecvType) (*RecvMessage, error) {
	for {
		msg, err := g.conn.Recv(ctx)
		if err != nil {
			return nil, fmt.Errorf("gateway recv: %w", err)
		}
		switch msg.Type {
		case RecvDocs, RecvTasks, RecvNamespaces, RecvQueues, RecvTime:
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

// makeDoModify makes a DoModify handler for this gateway that can be passed into a Run.
func (g *Gateway) makeDoModify() worker.DoModifyRun[json.RawMessage] {
	return func(ctx context.Context, _ entroq.Reader, _ *worker.Work[json.RawMessage]) (*worker.Result, error) {
		// The cue already sent the work. This phase only collects the answer,
		// and it is the half that runs under renewal -- which is the point of
		// the split: the client has the task, so holding the claim open for it
		// is right. Waiting for a client that never collected it would not be.
		received, err := g.recv(ctx, RecvModify)
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
	args, err := pbconv.ModifyArgsFromProto(mod.ModifyRequest, version.Protocol)
	if err != nil {
		return nil, err
	}
	return args, nil
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
		msg := g.newSend(SendWork, RecvModify)
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
		if g.config.SendSuccess {
			reported = g.report(ctx, g.newSend(SendSuccess, RecvError))
		}
		if g.quitRequested() {
			g.Close()
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
		if g.config.SendDependency {
			msg := g.newSend(SendDependency, RecvError)
			// Deps leads with a DETAIL entry carrying the message and then
			// names each dependency that failed, so there is nothing a Message
			// would add that a client is not better off reading from the list.
			msg.Deps = depsToWire(depErr)
			reported = g.report(ctx, msg)
		}
		if g.quitRequested() {
			g.Close()
			return nil
		}
		return reported
	}
}

// report sends a post-commit message and turns the client answer into the
// disposition the hook returns. Both hooks do the same thing with it, so they
// say it once.
func (g *Gateway) report(ctx context.Context, msg *SendMessage) error {
	if err := g.conn.Send(ctx, msg); err != nil {
		return fmt.Errorf("send %s: %w", msg.Type, err)
	}
	received, err := g.recv(ctx, RecvError)
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
		msg := g.newSend(SendTake, RecvTake)
		msg.Task = task
		if err := g.conn.Send(ctx, msg); err != nil {
			return nil, fmt.Errorf("send take docs: %w", err)
		}
		received, err := g.recv(ctx, RecvTake)
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
