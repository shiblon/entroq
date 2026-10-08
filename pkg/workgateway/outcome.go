package workgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
)

// Outcome is what a client reports for a task it was given.
//
// The five of them are the Go worker's dispositions and nothing else, so a
// worker on the wire has exactly the vocabulary a native one has. Retry and
// quarantine are ONE decision here, taken at the moment of failure, which is
// why a client says "retry" rather than re-queueing the task itself: a client
// that re-queued and then checked its own attempt ceiling on the next claim
// would quarantine an exhausted task only if some worker came back for it, and
// none may.
type Outcome string

const (
	// OutcomeOK commits the modification, which may be empty. The zero value,
	// so a client with nothing to say says nothing.
	OutcomeOK Outcome = ""
	// OutcomeRetry re-queues the task with backoff and counts the attempt,
	// quarantining it once the attempts run out.
	OutcomeRetry Outcome = "retry"
	// OutcomeMove quarantines the task now, to Error.Queue or to the session's
	// error queue when that is empty.
	OutcomeMove Outcome = "move"
	// OutcomeFatal stops the worker. The task keeps its claim and comes back
	// when the lease lapses.
	OutcomeFatal Outcome = "fatal"
	// OutcomeError is a handler failure the client does not understand well
	// enough to classify. The worker stops, quarantining the task first if this
	// claim was the last the claim limit allows.
	OutcomeError Outcome = "error"
)

// TaskError says why an outcome is not ok.
type TaskError struct {
	// Message is what went wrong, for a person reading a log.
	Message string `json:"message"`

	// Queue is where a move sends the task. Empty sends it to the session's
	// error queue, which is what a client that does not care should do: the
	// error queue is the deployment's to choose, not the worker's.
	Queue string `json:"queue,omitempty"`
}

func (e *TaskError) message() string {
	if e == nil || e.Message == "" {
		return "the client reported no reason"
	}
	return e.Message
}

func (e *TaskError) queue() string {
	if e == nil {
		return ""
	}
	return e.Queue
}

// disposition turns a client's outcome into the error the worker acts on, or
// nil for ok. The mapping is the point of the whole vocabulary: a client names
// a disposition and the worker performs it, so the attempt counting, the
// quarantine at the ceiling and the arrival backoff all stay in one place.
func (g *Gateway) disposition(outcome Outcome, te *TaskError) error {
	switch outcome {
	case OutcomeOK:
		return nil
	case OutcomeRetry:
		retry := worker.RetryErrorf("client asked to retry: %s", te.message())
		if d := g.retryDelay(); d > 0 {
			retry = retry.After(d)
		}
		if q := te.queue(); q != "" {
			// A retry that names a queue says where to go once the attempts
			// run out, rather than where to go now.
			retry = retry.OrMoveTo(q)
		}
		return retry
	case OutcomeMove:
		if q := te.queue(); q != "" {
			return worker.MoveErrorf("client asked to move: %s", te.message()).To(q)
		}
		return worker.MoveErrorf("client asked to move: %s", te.message())
	case OutcomeFatal:
		return worker.FatalErrorf("client reported a fatal error: %s", te.message())
	case OutcomeError:
		// Deliberately not a sentinel. An unclassified handler failure stops
		// the worker, and the worker quarantines the task first if this claim
		// was its last -- which is what a plain error means to runOne.
		return fmt.Errorf("client reported an error it could not classify: %s", te.message())
	default:
		return worker.FatalErrorf("unknown outcome %q", outcome)
	}
}

// retryDelay is the base delay a retried task waits before it is available
// again, or zero to leave the worker's own default alone.
func (g *Gateway) retryDelay() time.Duration {
	if g.config == nil || g.config.RetryDelayS <= 0 {
		return 0
	}
	return time.Duration(g.config.RetryDelayS) * time.Second
}

// classify says why the gateway stopped, in the one vocabulary a transport can
// act on: an exit code over a pipe, a status over HTTP (see ExitClass).
//
// It is rederived rather than carried over from protocol 1, whose version read
// a connLost flag off the bridge. There is no such flag now: a client that
// hangs up fails a Send or a Recv, and that is a clean stop because hanging up
// is how this protocol says goodbye.
func (g *Gateway) classify(err error) ExitClass {
	if err == nil {
		return ExitOK
	}
	// Cancellation is this session being told to stop, by Close or by the
	// transport going away; io.EOF is a pipe whose client is gone. Both are
	// how a client says goodbye, so neither is a fault. A transport reports a
	// hang-up in its own idiom rather than through a sentinel of ours.
	if entroq.IsCanceled(err) || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return ExitOK
	}
	g.Lock()
	closed := g.closed
	g.Unlock()
	if closed {
		return ExitOK
	}
	// An unreachable EntroQ will likely come back; reconnecting is worth it.
	if entroq.IsUnavailable(err) {
		return ExitTransient
	}
	// A fatal is either the client's own verdict or the gateway refusing what
	// the client sent. Replaying it replays the problem.
	if _, ok := worker.AsFatal(err); ok {
		return ExitCaller
	}
	return ExitGateway
}

// exit wraps why the gateway stopped with its class, so a transport reads the
// class instead of interpreting a Go error. A clean stop is a nil error.
func (g *Gateway) exit(err error) error {
	class := g.classify(err)
	if class == ExitOK {
		return nil
	}
	return &ExitError{Class: class, err: err}
}
