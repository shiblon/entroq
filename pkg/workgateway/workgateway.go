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
	Cleanup     bool     // worker implements the cleanup phase
}

// Bridge drives one worker connection. Run takes the worker's registration
// (Config) and runs the Go worker loop, translating each lifecycle phase into a
// protocol message and the reply back into worker behavior. One connection
// handles one task at a time, so the Bridge needs no locking; after Run begins
// its only state is the immutable Config, and all per-task state lives in a fresh
// handler the worker builds per task.
type Bridge struct {
	conn Conn
	cfg  Config
}

// NewBridge builds a Bridge over conn.
func NewBridge(conn Conn) *Bridge {
	return &Bridge{conn: conn}
}

// Run constructs a worker implementing exactly the phases the registration
// declares and runs it against eq until ctx is done or the worker stops. It
// fails loudly on a registration that cannot do useful work (no queues, or no
// work handler) rather than silently churning tasks. It returns nil on a clean
// context cancellation.
func (b *Bridge) Run(ctx context.Context, eq *entroq.EntroQ, cfg Config, lease time.Duration) error {
	if len(cfg.Queues) == 0 {
		return fmt.Errorf("gateway registration: at least one queue is required")
	}
	if !cfg.Work {
		return fmt.Errorf("gateway registration: a work handler is required (the gateway has nothing to do without one)")
	}
	b.cfg = cfg

	opts := []worker.Option[json.RawMessage]{}
	if cfg.TakeDocs {
		opts = append(opts, worker.WithTakeDocs[json.RawMessage](b.takeDocs))
	}
	opts = append(opts, worker.WithDoModify[json.RawMessage](b.doWork))

	w := worker.New(eq, opts...)
	return w.Run(ctx,
		worker.Watching(cfg.Queues...),
		worker.WithLease(lease),
		worker.WithMaxAttempts(cfg.MaxAttempts),
	)
}

// takeDocs runs the TakeDocs phase: ask the client which docs the task needs and
// return them for the gateway to claim. Wired only when the client registered
// takeDocs.
func (b *Bridge) takeDocs(ctx context.Context, task *entroq.Task, _ json.RawMessage) ([]*entroq.DocClaim, error) {
	if err := b.conn.Send(ctx, takeDocsMsg{Type: msgTakeDocs, Task: task}); err != nil {
		return nil, fmt.Errorf("send takeDocs: %w", err)
	}
	var d docsMsg
	if err := b.conn.Recv(ctx, &d); err != nil {
		return nil, fmt.Errorf("read docs: %w", err)
	}
	claims := make([]*entroq.DocClaim, 0, len(d.Claims))
	for _, c := range d.Claims {
		claims = append(claims, entroq.ClaimKey(c.Namespace, c.Key))
	}
	return claims, nil
}

// doWork runs the DoWork phase: hand the task and any docs to the client and
// translate its reply into a modification to commit or a structured worker
// error. When the client registered cleanup, a successful result chains an
// OnSuccess step that runs the cleanup phase after the commit.
func (b *Bridge) doWork(ctx context.Context, task *entroq.Task, _ json.RawMessage, docs []*entroq.Doc) (*worker.Result, error) {
	if err := b.conn.Send(ctx, doWorkMsg{Type: msgDoWork, Task: task, Docs: docs}); err != nil {
		return nil, fmt.Errorf("send doWork: %w", err)
	}
	var res result
	if err := b.conn.Recv(ctx, &res); err != nil {
		return nil, fmt.Errorf("read result: %w", err)
	}

	switch res.Outcome {
	case outcomeOK:
		args, err := res.Modification.modifyArgs()
		if err != nil {
			// A malformed modification is a client bug, not a transient fault:
			// retrying would only replay the same bad message, so stop the worker.
			return nil, worker.FatalErrorf("invalid modification from worker: %v", err)
		}
		r := worker.Modify(args...)
		if b.cfg.Cleanup {
			r = r.OnSuccess(b.cleanup)
		}
		return r, nil
	case outcomeRetry:
		re := worker.RetryErrorf("%s", orDefault(res.Message, "worker requested retry"))
		if res.After != "" {
			d, err := time.ParseDuration(res.After)
			if err != nil {
				return nil, worker.FatalErrorf("bad retry 'after' %q: %v", res.After, err)
			}
			re = re.After(d)
		}
		if res.OrMove != "" {
			re = re.OrMoveTo(res.OrMove)
		}
		return nil, re
	case outcomeMove:
		me := worker.MoveErrorf("%s", orDefault(res.Message, "worker requested move"))
		if res.To != "" {
			me = me.To(res.To)
		}
		return nil, me
	case outcomeFatal:
		return nil, worker.FatalErrorf("%s", orDefault(res.Message, "worker requested fatal"))
	default:
		return nil, worker.FatalErrorf("unknown result outcome %q", res.Outcome)
	}
}

// cleanup runs the Cleanup phase after a successful commit: tell the client the
// task committed and let it run a best-effort post-commit step. A "fatal" reply
// stops the worker (OnSuccess returning a FatalError); anything else continues.
// The task is already committed, so this step is at-most-once by nature.
func (b *Bridge) cleanup(ctx context.Context) error {
	if err := b.conn.Send(ctx, cleanupMsg{Type: msgCleanup}); err != nil {
		return fmt.Errorf("send cleanup: %w", err)
	}
	var d done
	if err := b.conn.Recv(ctx, &d); err != nil {
		return fmt.Errorf("read done: %w", err)
	}
	if d.Outcome == outcomeFatal {
		return worker.FatalErrorf("%s", orDefault(d.Message, "worker cleanup requested fatal"))
	}
	return nil
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
