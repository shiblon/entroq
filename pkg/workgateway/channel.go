package workgateway

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// ChannelConn carries the protocol over two channels instead of a stream, for a
// transport whose messages arrive already decoded -- an HTTP service, which has
// to read a request body anyway to find its type and session.
//
// The channels are UNBUFFERED by intent, and the whole conn rests on that. A
// Send does not return until the far end has taken the message, which is what
// makes two different things true: a cue that nobody collects leaves the
// gateway holding a task it has not renewed (see worker.Handler.CueWork), and a
// refusal cannot be lost to the Close that follows it.
//
// Nothing here is a turn. Turn-taking is the gateway's: its phases are
// sequential, so whichever one is running is the only thing that could be
// speaking.
type ChannelConn struct {
	sync.Mutex

	r <-chan *Request
	w chan<- *Response

	done chan bool
}

// NewChannelConn makes a conn over a request channel and a response channel.
// Both are from the GATEWAY's perspective: it receives from r and sends on w.
//
// The transport keeps the other ends. It writes a decoded request to r and
// reads the answer from w, which is exactly one request/response exchange of
// whatever it is carrying them over.
func NewChannelConn(r <-chan *Request, w chan<- *Response) *ChannelConn {
	return &ChannelConn{
		r:    r,
		w:    w,
		done: make(chan bool),
	}
}

// Recv waits for the next request the transport forwards.
func (c *ChannelConn) Recv(ctx context.Context) (*Request, error) {
	select {
	case <-c.done:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, fmt.Errorf("recv canceled (%v): %w", context.Cause(ctx), ctx.Err())
	case msg := <-c.r:
		return msg, nil
	}
}

// Send hands a response to the transport, waiting until it is taken.
func (c *ChannelConn) Send(ctx context.Context, msg *Response) error {
	select {
	case <-c.done:
		return io.EOF
	case <-ctx.Done():
		return fmt.Errorf("send canceled (%v): %w", context.Cause(ctx), ctx.Err())
	case c.w <- msg:
		return nil
	}
}

// Close ends the conversation and makes Send and Recv report EOF.
//
// It closes neither channel. The transport owns them, and a closed data channel
// would hand a nil request to whoever read it next; done is a signal, which is
// the only kind of channel it is safe to close.
func (c *ChannelConn) Close() error {
	c.Lock()
	defer c.Unlock()
	select {
	case <-c.done:
		return io.EOF
	default:
	}
	close(c.done)
	return nil
}
