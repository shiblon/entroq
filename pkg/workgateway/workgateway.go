// Package workgateway bridges EntroQ's worker loop to a language-agnostic worker
// spoken to over a simple newline-delimited JSON protocol. eqlink runs the hard
// part (claim, renew, commit) and, for each claimed task, sends the worker a
// "work" message and reads back one "result"; the worker never touches EntroQ,
// gRPC, or the queue protocol. A client is therefore trivial to implement in any
// language: read a tagged JSON object, do the work, write a tagged JSON object.
//
// The protocol is transport-agnostic. This skeleton carries it over a stdio pipe
// (eqlink work as a child process); a WebSocket transport will carry the same
// messages later. Because json.Encoder appends a newline, the wire is one
// compact JSON object per line (ndjson), which every language reads trivially.
//
// One connection is one worker slot: exactly one task in flight, strict
// request/response, no correlation ids. Concurrency is more connections.
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

// workMsg is sent to the worker for each claimed task (the "work" phase). The
// task is the full entroq.Task, the identical value an in-process Go worker's
// DoWork receives (its Value stays raw JSON), so a wire worker in any language
// sees exactly what a native one does, down to fields like FromQueue and
// Attempt that a reaper or authorizer might care about.
type workMsg struct {
	Type string       `json:"type"` // always "work"
	Task *entroq.Task `json:"task"`
}

// result is the worker's reply to a work message: exactly one outcome.
type result struct {
	Type    string `json:"type"`              // "result"
	Outcome string `json:"outcome"`           // ok | retry | move | fatal
	Message string `json:"message,omitempty"` // error detail for retry/move/fatal
	After   string `json:"after,omitempty"`   // retry: delay before re-arrival, e.g. "30s"
	OrMove  string `json:"orMove,omitempty"`  // retry: quarantine queue once attempts exhaust
	To      string `json:"to,omitempty"`      // move: destination queue
}

// Conn carries the protocol's JSON messages over some transport, one message
// per Send and per Recv. A stdio pipe (PipeConn) and a WebSocket are both just
// Conns; that is what keeps the Bridge transport-agnostic.
type Conn interface {
	Send(ctx context.Context, v any) error // marshal and write one protocol message
	Recv(ctx context.Context, v any) error // read and unmarshal the next message into v
}

// Bridge speaks the work protocol over a Conn. One connection handles one task
// at a time, so no locking is needed.
type Bridge struct {
	conn Conn
}

// NewBridge builds a Bridge over conn.
func NewBridge(conn Conn) *Bridge {
	return &Bridge{conn: conn}
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

// Send writes v as one newline-terminated JSON message. It ignores ctx: the
// json encoder is not context-aware, and a stdio pipe is canceled by closing it.
func (c *PipeConn) Send(_ context.Context, v any) error { return c.enc.Encode(v) }

// Recv decodes the next JSON message into v. It ignores ctx (see Send).
func (c *PipeConn) Recv(_ context.Context, v any) error { return c.dec.Decode(v) }

// DoWork is the worker.DoModifyRun for the gateway. It hands the task to the
// worker over the wire and translates the reply into modifications or a
// structured worker error. A broken pipe (worker gone) surfaces as an error
// that ends the loop, leaving the claimed task to time out and be reclaimed.
func (b *Bridge) DoWork(ctx context.Context, task *entroq.Task, _ json.RawMessage, _ []*entroq.Doc) ([]entroq.ModifyArg, error) {
	if err := b.conn.Send(ctx, workMsg{Type: "work", Task: task}); err != nil {
		return nil, fmt.Errorf("send work: %w", err)
	}
	var res result
	if err := b.conn.Recv(ctx, &res); err != nil {
		return nil, fmt.Errorf("read result: %w", err)
	}

	switch res.Outcome {
	case "ok":
		// Skeleton: success means consume the input. Full declarative
		// modifications land here later.
		return []entroq.ModifyArg{task.Delete()}, nil
	case "retry":
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
	case "move":
		me := worker.MoveErrorf("%s", orDefault(res.Message, "worker requested move"))
		if res.To != "" {
			me = me.To(res.To)
		}
		return nil, me
	case "fatal":
		return nil, worker.FatalErrorf("%s", orDefault(res.Message, "worker requested fatal"))
	default:
		return nil, worker.FatalErrorf("unknown result outcome %q", res.Outcome)
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
