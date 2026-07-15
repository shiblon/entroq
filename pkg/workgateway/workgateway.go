// Package workgateway bridges EntroQ's worker loop to a language-agnostic worker
// spoken to over a small newline-delimited JSON protocol. eqlink runs the hard,
// stateful part (claim, renew at half the lease, stop-and-freeze before commit,
// version fix-up, retry/move/backoff, doc-claim ordering) once, in Go; a worker
// in any language connects, registers the queues it serves and the phases it
// implements, and then answers phase messages. It never touches EntroQ, gRPC, or
// the queue protocol. See protocol.go for the full wire contract.
//
// The protocol is transport-agnostic: a Conn is any one-message-per-Send,
// one-message-per-Recv channel. PipeConn carries it over a stdio pipe (the
// primary transport, e.g. eqlink work as a child process) and WSConn carries the
// identical messages over a WebSocket. One connection is one worker slot:
// exactly one task in flight, strict request/response, no correlation ids;
// concurrency is more connections.
package workgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/pbconv"
	"github.com/shiblon/entroq/pkg/worker"
)

// Conn carries the protocol's JSON messages over some transport, one message per
// Send and per Recv. A stdio pipe (PipeConn) and a WebSocket (WSConn) are both
// just Conns; that is what keeps the Bridge transport-agnostic.
type Conn interface {
	Send(ctx context.Context, v any) error // marshal and write one protocol message
	Recv(ctx context.Context, v any) error // read and unmarshal the next message into v
}

// Config is a worker's registration, supplied by the transport out-of-band at
// connection time (flags/env for a spawned pipe gateway, URL params/headers for
// a WebSocket connection), never as a wire message. It is connection-scoped and
// fixed for the session. The lease is deliberately not here: it governs renewal
// cadence and reclaim latency, operational concerns owned by whoever runs the
// gateway, not by a connecting client.
type Config struct {
	Queues      []string // queues the worker serves (at least one required)
	MaxAttempts int32    // 0 means unlimited
	TakeDocs    bool     // worker implements the takeDocs phase
	Work        bool     // worker implements the work phase (required)
	Success     bool     // worker implements the success phase (post-commit)
	Dependency  bool     // worker implements the dependency phase (commit lost a dependency)
}

// defaultEntroQTimeout is how long a Bridge rides out an unreachable EntroQ
// backend before giving up, unless WithEntroQTimeout overrides it.
const defaultEntroQTimeout = 60 * time.Second

// Bridge drives one worker connection. It runs the Go worker loop, translating
// each lifecycle phase into a protocol message and the reply back into worker
// behavior, and supervises that loop: it reconnects across a transient EntroQ
// outage (bounded by the fatal timeout) and classifies why it stops so the
// transport can report an exit code or close code. One connection handles one
// task at a time, so the Bridge needs no locking; its configuration is fixed at
// construction and all per-task state lives in a fresh handler the worker builds
// per task. connLost is the single bit of run-to-run state: once a Send or Recv
// fails, the client is gone and there is nothing left to serve.
type Bridge struct {
	conn          Conn
	cfg           Config
	lease         time.Duration
	entroqTimeout time.Duration
	connLost      bool
}

// Option configures a Bridge at construction.
type Option func(*Bridge)

// WithConfig sets the worker's registration: the queues it serves, its
// max-attempts, and which phases it implements. It is the usual starting point,
// since each transport assembles the registration from its out-of-band preamble
// (flags for a pipe, URL params for a WebSocket) into one Config.
func WithConfig(cfg Config) Option {
	return func(b *Bridge) { b.cfg = cfg }
}

// WithLease sets the claim lease and renewal cadence. It is deliberately
// separate from the registration: the lease governs reclaim latency and is the
// gateway operator's concern, not the connecting worker's. It defaults to
// entroq.DefaultClaimDuration.
func WithLease(d time.Duration) Option {
	return func(b *Bridge) { b.lease = d }
}

// WithEntroQTimeout bounds how long the gateway rides out an unreachable EntroQ
// backend (one being restarted or relocated by an orchestrator) before giving up
// and exiting Transient. Within the window it reconnects with backoff,
// transparently to the client, which sees a pause rather than a disconnect. Zero
// disables the ride-out entirely: an EntroQ blip becomes an immediate Transient
// exit for the client's supervisor to handle. Defaults to defaultEntroQTimeout.
func WithEntroQTimeout(d time.Duration) Option {
	return func(b *Bridge) { b.entroqTimeout = d }
}

// NewBridge builds a Bridge over conn, applying opts. The lease defaults to
// entroq.DefaultClaimDuration and the EntroQ timeout to defaultEntroQTimeout;
// supply WithConfig to register the worker.
func NewBridge(conn Conn, opts ...Option) *Bridge {
	b := &Bridge{conn: conn, lease: entroq.DefaultClaimDuration, entroqTimeout: defaultEntroQTimeout}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// send and recv wrap the Conn and record a lost connection. Every phase goes
// through them, so classify can tell "the client hung up" from any other stop by
// inspecting one bit, no matter how the error was wrapped on the way out.
func (b *Bridge) send(ctx context.Context, v any) error {
	if err := b.conn.Send(ctx, v); err != nil {
		b.connLost = true
		return err
	}
	return nil
}

func (b *Bridge) recv(ctx context.Context, v any) error {
	if err := b.conn.Recv(ctx, v); err != nil {
		b.connLost = true
		return err
	}
	return nil
}

// Run supervises the worker loop against eq until ctx is done or a terminal
// condition stops it. It fails loudly on a registration that cannot do useful
// work (no queues, or no work handler). While the worker runs, a transient
// EntroQ outage (the backend restarting or relocating) is ridden out with
// backoff up to the fatal timeout, transparently to the client; anything else
// classifies the stop, reports it to the client over the error channel when the
// connection is still alive, and returns. It returns nil for a clean stop
// (graceful shutdown, or the client hanging up) and an *ExitError otherwise, so
// the transport can map the class onto an exit or close code.
func (b *Bridge) Run(ctx context.Context, eq *entroq.EntroQ) error {
	if len(b.cfg.Queues) == 0 {
		return &ExitError{Class: ExitCaller, err: fmt.Errorf("gateway registration: at least one queue is required")}
	}
	if !b.cfg.Work {
		return &ExitError{Class: ExitCaller, err: fmt.Errorf("gateway registration: a work handler is required (the gateway has nothing to do without one)")}
	}

	const (
		minBackoff = 200 * time.Millisecond
		maxBackoff = 5 * time.Second
	)
	var (
		outageStart time.Time
		backoff     = minBackoff
	)
	for {
		runStart := time.Now()
		err := b.runWorker(ctx, eq)
		class := b.classify(err)

		switch class {
		case ExitOK:
			return nil
		case ExitCaller, ExitGateway:
			b.notify(ctx, class, err)
			return &ExitError{Class: class, err: err}
		}

		// ExitTransient: EntroQ is unreachable. The client connection is fine, so
		// tell the client we are retrying, then ride the outage out with backoff,
		// bounded by the fatal timeout.
		b.notify(ctx, ExitTransient, err)
		if b.connLost {
			return nil // the client hung up while we were reporting; stop cleanly
		}
		if b.entroqTimeout <= 0 {
			return &ExitError{Class: ExitTransient, err: err} // ride-out disabled
		}
		// A run that lasted longer than the whole timeout clearly had uptime before
		// failing, so treat this as a fresh outage rather than an old one continuing.
		if time.Since(runStart) > b.entroqTimeout {
			outageStart, backoff = time.Time{}, minBackoff
		}
		if outageStart.IsZero() {
			outageStart = runStart
		}
		if time.Since(outageStart) > b.entroqTimeout {
			return &ExitError{Class: ExitTransient, err: fmt.Errorf("entroq unavailable for more than %s: %w", b.entroqTimeout, err)}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runWorker builds a worker for exactly the registered phases and runs it once,
// until ctx is done or it stops. The supervision loop in Run calls it repeatedly,
// reusing this Bridge (and its connection) across restarts.
func (b *Bridge) runWorker(ctx context.Context, eq *entroq.EntroQ) error {
	opts := []worker.Option[json.RawMessage]{}
	if b.cfg.TakeDocs {
		opts = append(opts, worker.WithTakeDocs[json.RawMessage](b.takeDocs))
	}
	opts = append(opts, worker.WithDoModify[json.RawMessage](b.doWork))

	w := worker.New(eq, opts...)
	return w.Run(ctx,
		worker.Watching(b.cfg.Queues...),
		worker.WithLease(b.lease),
		worker.WithMaxAttempts(b.cfg.MaxAttempts),
	)
}

// classify maps a worker run's exit into a class. The connLost bit is checked
// before everything except a clean stop: a dropped client connection may also
// surface wrapped as a fatal (the success phase escalates it to stop the worker
// promptly), but it is still just the client hanging up, which is a clean end of
// service, not a fault or a thing to retry.
func (b *Bridge) classify(err error) ExitClass {
	switch {
	case err == nil || entroq.IsCanceled(err):
		return ExitOK
	case b.connLost:
		return ExitOK
	case entroq.IsUnavailable(err):
		return ExitTransient
	default:
		if _, ok := worker.AsFatal(err); ok {
			return ExitCaller
		}
		return ExitGateway
	}
}

// notify reports a non-clean stop to the client over the still-open connection:
// a transient backend blip it is retrying, or the cause of a caller/gateway exit
// just before it returns. It is one-way and best-effort -- the client acts on it
// rather than replying, and if the connection is already gone there is no one to
// tell (the send fails, records the loss, and is ignored).
func (b *Bridge) notify(ctx context.Context, class ExitClass, cause error) {
	if b.connLost {
		return
	}
	_ = b.send(ctx, errorMsg{Type: msgError, Class: class.String(), Message: cause.Error()})
}

// takeDocs runs the TakeDocs phase: ask the client which docs the task needs and
// return them for the gateway to claim. Wired only when the client registered
// takeDocs.
func (b *Bridge) takeDocs(ctx context.Context, task *entroq.Task, _ json.RawMessage) ([]*entroq.DocClaim, error) {
	taskPB, err := pbconv.TaskToProto(task)
	if err != nil {
		return nil, fmt.Errorf("convert task for takeDocs: %w", err)
	}
	if err := b.send(ctx, takeDocsMsg{Type: msgTakeDocs, Task: wireTask{taskPB}}); err != nil {
		return nil, fmt.Errorf("send takeDocs: %w", err)
	}
	var d docsMsg
	if err := b.recv(ctx, &d); err != nil {
		return nil, fmt.Errorf("read docs: %w", err)
	}
	if d.Type != msgDocs {
		return nil, fmt.Errorf("expected %q message, got %q", msgDocs, d.Type)
	}
	claims := make([]*entroq.DocClaim, 0, len(d.Claims))
	for _, c := range d.Claims {
		claims = append(claims, entroq.ClaimKey(c.Namespace, c.Key))
	}
	return claims, nil
}

// doWork runs the DoWork phase: hand the task and any docs to the client and
// translate its reply into a modification to commit or a structured worker
// error. It chains the post-commit phases the worker registered: OnSuccess runs
// the success phase after a good commit, OnDependency the dependency phase when
// the commit loses a dependency race.
func (b *Bridge) doWork(ctx context.Context, task *entroq.Task, _ json.RawMessage, docs []*entroq.Doc) (*worker.Result, error) {
	taskPB, err := pbconv.TaskToProto(task)
	if err != nil {
		return nil, fmt.Errorf("convert task for doWork: %w", err)
	}
	msg := doWorkMsg{Type: msgDoWork, Task: wireTask{taskPB}}
	for _, d := range docs {
		docPB, err := pbconv.DocToProto(d)
		if err != nil {
			return nil, fmt.Errorf("convert doc for doWork: %w", err)
		}
		msg.Docs = append(msg.Docs, wireDoc{docPB})
	}
	if err := b.send(ctx, msg); err != nil {
		return nil, fmt.Errorf("send doWork: %w", err)
	}
	var res result
	if err := b.recv(ctx, &res); err != nil {
		return nil, fmt.Errorf("read result: %w", err)
	}
	if res.Type != msgResult {
		// A wrong message type is a client protocol bug, not a transient fault.
		return nil, worker.FatalErrorf("expected %q message, got %q", msgResult, res.Type)
	}
	// A retry/move/fatal outcome maps straight to the worker sentinel it names.
	if serr, ok := res.sentinel(); ok {
		return nil, serr
	}

	// Outcome "ok": commit the modification. An absent modification commits
	// nothing and leaves the task to be reclaimed after its lease, faithful to a
	// Go DoModify that returns no mods.
	var args []entroq.ModifyArg
	if res.Modification != nil {
		args, err = pbconv.ModifyArgsFromProto(res.Modification.ModifyRequest)
		if err != nil {
			// A malformed modification is a client bug, not a transient fault:
			// retrying would only replay the same bad message, so stop the worker.
			// (pbconv flags a caller-fixable request as *InvalidRequestError; over
			// the gateway that too is just a client protocol bug.)
			return nil, worker.FatalErrorf("invalid modification from worker: %v", err)
		}
	}
	// The ack shorthand deletes the claimed task, unless the modification already
	// disposes of it: an explicit change/delete/depend on the claimed id wins, so
	// ack is a forgiving "I'm done with this" default rather than a conflict. The
	// gateway deletes from its own claimed task; the worker's Finish fixes the
	// version up to the stable renewed value.
	if res.Ack && !modificationTouches(res.Modification, task.ID) {
		args = append(args, entroq.NewTaskID(task.ID, task.Version, task.Queue).Delete())
	}
	// Attach only the post-commit phases the worker registered: OnSuccess runs the
	// success phase after a good commit; OnDependency lets the worker pick the
	// task's fate if the commit loses a dependency race. An unregistered phase
	// simply does not fire (a dependency failure then reclaims on lease expiry).
	r := worker.Modify(args...)
	if b.cfg.Success {
		r = r.OnSuccess(b.success)
	}
	if b.cfg.Dependency {
		r = r.OnDependency(b.report)
	}
	return r, nil
}

// modificationTouches reports whether the modification disposes of the task with
// the given id via a change, delete, or depend. It decides whether the ack
// shorthand is suppressed (see result.Ack): an explicit op on the claimed task
// beats the shorthand.
func modificationTouches(m *wireModReq, id string) bool {
	if m == nil {
		return false
	}
	for _, ch := range m.Changes {
		if ch.GetOldId().GetId() == id {
			return true
		}
	}
	for _, del := range m.Deletes {
		if del.GetId() == id {
			return true
		}
	}
	for _, dep := range m.Depends {
		if dep.GetId() == id {
			return true
		}
	}
	return false
}

// report runs the OnDependency phase: the commit failed a dependency check, so
// tell the client exactly which task and doc dependencies failed and let it pick
// the task's disposition. The reply is honored optimistically by the worker
// (the commit already failed and renewal has stopped), so a retry/move lands
// only if the task itself was not implicated; "ok" leaves it to be reclaimed on
// lease expiry.
func (b *Bridge) report(ctx context.Context, depErr *entroq.DependencyError) error {
	msg := dependencyMsg{Type: msgDependency}
	for _, d := range pbconv.DependencyErrorDetails(depErr) {
		msg.Deps = append(msg.Deps, wireDep{d})
	}
	if err := b.send(ctx, msg); err != nil {
		// A transport failure here is a dead connection, not a task problem.
		// Returning a plain (non-sentinel) error exits the worker, the right
		// response to a broken pipe (see the worker's exit-on-unknown ladder).
		return fmt.Errorf("send dependency: %w", err)
	}
	var d done
	if err := b.recv(ctx, &d); err != nil {
		return fmt.Errorf("read done: %w", err)
	}
	if d.Type != msgDone {
		return worker.FatalErrorf("expected %q message, got %q", msgDone, d.Type)
	}
	// "ok"/empty yields a nil sentinel: leave the task to be reclaimed on lease
	// expiry. Any other outcome returns the retry/move/fatal sentinel, which
	// OnDependency honors optimistically.
	serr, _ := d.sentinel()
	return serr
}

// success runs the success phase after a good commit: tell the client the task
// committed and let it run a best-effort post-commit step. The task is already
// committed, so this step is at-most-once by nature. It hands the done reply's
// disposition back to the worker's OnSuccess layer, which logs a non-fatal
// outcome and continues, stopping only on "fatal" -- so "retry"/"move" here are
// harmless no-ops, exactly as the Go OnSuccess contract states.
//
// A transport failure is the exception: because OnSuccess treats a plain error
// as best-effort and would loop on to claim another task the dead connection
// cannot deliver (needlessly starving that task for a lease period), a dropped
// connection must escalate to a FatalError so the worker stops instead.
func (b *Bridge) success(ctx context.Context) error {
	if err := b.send(ctx, successMsg{Type: msgSuccess}); err != nil {
		return worker.FatalErrorf("send success: %v", err)
	}
	var d done
	if err := b.recv(ctx, &d); err != nil {
		return worker.FatalErrorf("read done: %v", err)
	}
	if d.Type != msgDone {
		return worker.FatalErrorf("expected %q message, got %q", msgDone, d.Type)
	}
	serr, _ := d.sentinel()
	return serr
}

// PipeConn carries the protocol over a byte stream as newline-delimited JSON
// (json.Encoder appends the newline), e.g. a stdio pipe.
type PipeConn struct {
	enc *json.Encoder
	dec *json.Decoder
}

// NewPipeConn reads messages from r and writes them to w.
func NewPipeConn(r io.Reader, w io.Writer) *PipeConn {
	return &PipeConn{enc: json.NewEncoder(w), dec: json.NewDecoder(r)}
}

// Send writes v as one newline-terminated JSON message. It ignores ctx: the json
// encoder is not context-aware, and a stdio pipe is canceled by closing it.
func (c *PipeConn) Send(_ context.Context, v any) error { return c.enc.Encode(v) }

// Recv decodes the next JSON message into v. It ignores ctx (see Send).
func (c *PipeConn) Recv(_ context.Context, v any) error { return c.dec.Decode(v) }

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
