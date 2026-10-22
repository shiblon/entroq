package workgateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/sync/errgroup"
)

// DefaultMaxFrameBytes bounds one frame, newline included, unless
// WithMaxFrameBytes says otherwise. It matches eqserve's --max_size_mb, so a
// client whose messages fit over gRPC fits here too.
const DefaultMaxFrameBytes = 10 << 20

// ErrFrameTooLarge is what a frame over the limit gets. It ends the session:
// a line abandoned part-read leaves no resync point, since the newline that
// would mark the next frame is somewhere in the bytes nobody is reading.
var ErrFrameTooLarge = errors.New("frame too large")

// ErrMalformedFrame is a frame that does not decode as a request. Like a frame
// over the limit, it ends the session, and the peer is told why.
var ErrMalformedFrame = errors.New("malformed frame")

// StreamConn carries a gateway session over one byte stream: a unix socket, a
// TCP conn, anything that reads and writes. Two one-way streams, which is what
// a spawned child gets for its stdin and stdout, become one with Joining.
// Send is from the gateway's perspective, as is Recv.
//
// Two pumps move the bytes, because neither a blocking read nor a blocking
// write answers to a context. The reader pump frames and decodes, handing
// requests to Recv over a channel; the writer pump takes responses from Send
// and writes them. Cancellation is then a select on a channel, which contexts
// do cleanly, and Close is what stops a pump already blocked in a syscall, by
// closing the stream underneath it.
//
// The channels are unbuffered, so a pump runs at most one message ahead and a
// request nobody collected is still there for the next Recv.
type StreamConn struct {
	mu sync.Mutex

	// done closes when the session has ended, for whatever reason. ended says
	// which reason, and is set before this closes. Both are guarded by mu.
	done  chan struct{}
	ended error

	// gone closes when the reader pump stops, before it hands over its last
	// frame, so it closes whether or not anyone ever takes that frame.
	gone chan struct{}

	rCh <-chan requestFrame
	wCh chan<- responseFrame

	// stop ends both pumps, and closing the stream is what unblocks one that
	// is already in a syscall. closeStream reports the same result however
	// many times it is called, so the teardown path and Close can share it --
	// and it closes ONCE, which is why the stream arrives already joined.
	stop        context.CancelFunc
	closeStream func() error
}

// requestFrame is one thing off the wire: a request, or the error that ended the
// stream. Errors ride the same channel as requests so they arrive in order --
// after the last good request rather than instead of it, which is the
// difference between a malformed frame and a clean hang-up.
type requestFrame struct {
	req *Request
	err error
}

// responseFrame is one response and somewhere to report what became of it. Send
// waits for that verdict, so a nil return means the peer has the bytes rather
// than meaning a pump accepted them and may yet drop the session's last word.
type responseFrame struct {
	resp *Response
	ack  chan<- error
}

// Joining makes one two-way stream out of two one-way ones, which is what a
// spawned child gets for its stdin and stdout. Closing the result closes both,
// once, however many times it is called.
func Joining(r io.ReadCloser, w io.WriteCloser) io.ReadWriteCloser {
	return &joined{
		r: r,
		w: w,
		// idempotent double close - returns nil every time it's called if the
		// first close was a clean shutdown.
		closeOnce: sync.OnceValue(func() error {
			// The write side goes first, so a peer still reading sees the end
			// of the stream rather than a reader that vanished.
			return errors.Join(w.Close(), r.Close())
		}),
	}
}

// joined is the two one-way streams Joining made one of.
type joined struct {
	r         io.ReadCloser
	w         io.WriteCloser
	closeOnce func() error
}

func (j *joined) Read(p []byte) (int, error)  { return j.r.Read(p) }
func (j *joined) Write(p []byte) (int, error) { return j.w.Write(p) }
func (j *joined) Close() error                { return j.closeOnce() }

// NewStreamConn creates a connection over one byte stream for a Gateway, with
// the pumps already running. Close it to end the session and stop them.
//
// Closing the conn closes the stream exactly once, so a transport that is two
// streams says so with Joining rather than being passed twice.
//
// It takes the same options Serve does and uses the ones about framing, so an
// operator's one list goes to both.
func NewStreamConn(rwc io.ReadWriteCloser, options ...Option) *StreamConn {
	opts := resolveOptions(options)

	ctx, cancel := context.WithCancel(context.Background())
	rCh := make(chan requestFrame)
	wCh := make(chan responseFrame)

	c := &StreamConn{
		done:        make(chan struct{}),
		gone:        make(chan struct{}),
		rCh:         rCh,
		wCh:         wCh,
		stop:        cancel,
		closeStream: sync.OnceValue(rwc.Close),
	}

	g, gctx := errgroup.WithContext(ctx)

	// A pump blocked in a syscall answers to nothing but a closed stream, so
	// cancelling closes it. errgroup cancels gctx as soon as either pump
	// fails, which is how one pump's failure releases the other. The close
	// result is kept for the collector below to report.
	context.AfterFunc(gctx, func() {
		c.closeStream()
	})

	g.Go(func() error {
		return pumpRead(gctx, rwc, rCh, c.gone, opts.maxFrameBytes)
	})
	g.Go(func() error {
		return pumpWrite(gctx, rwc, wCh)
	})

	// Both pumps have returned by the time Wait does, so there is exactly one
	// place that decides why a session ended.
	go func() {
		c.finish(errors.Join(g.Wait(), c.closeStream()))
	}()

	return c
}

// pumpRead frames and decodes requests from r, handing each one to Recv.
//
// A frame is one JSON value on one line. JSON escapes the newlines inside
// strings, so the terminator is unambiguous, and a shell client can take one
// frame with a single `read -r`.
//
// bufio.Reader rather than json.Decoder because a Decoder keeps its first read
// error forever: it cannot report a bad frame and carry on, and it cannot be
// interrupted and resumed.
func pumpRead(ctx context.Context, r io.Reader, out chan<- requestFrame, gone chan<- struct{}, max int) error {
	// end is the read side's last word. Nothing further can arrive, which is
	// said first to anyone watching gone, and then handed to Recv in order.
	end := func(err error) error {
		close(gone)
		deliver(ctx, out, requestFrame{err: err})
		return err
	}
	br := bufio.NewReader(r)
	for {
		line, err := readFrame(br, max)
		if errors.Is(err, ErrFrameTooLarge) {
			// The peer is still there and reading, so only the read side ends:
			// the gateway can still answer, and tell it why.
			end(err)
			return nil
		}
		if err != nil {
			// A broken read leaves a fragment, not a frame, and the read error
			// is why the session is ending -- not whatever the fragment fails
			// to parse as, and not a request it may happen to parse into.
			if !errors.Is(err, io.EOF) {
				return end(fmt.Errorf("read frame: %w", err))
			}
			// A peer that hung up without its last newline still said
			// something whole: that line is decoded below, and the next read
			// reports the end.
			if len(line) == 0 {
				return end(err)
			}
		}
		req := new(Request)
		if uerr := json.Unmarshal(line, req); uerr != nil {
			// As for a frame over the limit, only the read side ends.
			end(fmt.Errorf("%w: %v", ErrMalformedFrame, uerr))
			return nil
		}
		if derr := deliver(ctx, out, requestFrame{req: req}); derr != nil {
			return derr
		}
	}
}

// readFrame reads one newline-terminated frame, refusing one over max bytes.
// A max of zero or less is no bound.
//
// ReadSlice rather than ReadBytes, which grows a buffer for as long as a peer
// withholds the newline: the bounded pieces are accumulated here instead, and
// the count is checked before each one lands.
func readFrame(br *bufio.Reader, max int) ([]byte, error) {
	var frame []byte
	for {
		piece, err := br.ReadSlice('\n')
		if max > 0 && len(frame)+len(piece) > max {
			return nil, fmt.Errorf("%w: over %d bytes", ErrFrameTooLarge, max)
		}
		frame = append(frame, piece...)
		// A full buffer means the newline is further along, not that the read
		// failed, so the next ReadSlice carries on where this one stopped.
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return frame, err
	}
}

// pumpWrite writes responses from in to w, one newline-terminated frame each.
//
// json.Marshal and an explicit newline rather than json.Encoder, so the frame
// the gateway writes is exactly the frame pumpRead demands.
func pumpWrite(ctx context.Context, w io.Writer, in <-chan responseFrame) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out := <-in:
			err := writeFrame(w, out.resp)
			// The ack is buffered, so a Send that gave up waiting does not
			// wedge the pump holding a verdict nobody wants.
			out.ack <- err
			if err != nil {
				return err
			}
		}
	}
}

// writeFrame writes one response as a newline-terminated frame.
func writeFrame(w io.Writer, resp *Response) error {
	frame, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("encode %q: %w", resp.Type, err)
	}
	if _, err := w.Write(append(frame, '\n')); err != nil {
		return fmt.Errorf("write %q: %w", resp.Type, err)
	}
	return nil
}

// deliver hands one requestFrame to Recv, giving up if the session ends first.
//
// A value nobody collects is not a failure of its own: the gateway may have
// moved on, and the pump still has to be able to exit.
func deliver(ctx context.Context, out chan<- requestFrame, f requestFrame) error {
	select {
	case out <- f:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// finish records why the session ended and releases everyone waiting on it. A
// session the caller closed on purpose ends cleanly: the pumps' complaints
// about streams that are gone are teardown noise, not the reason.
func (c *StreamConn) finish(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return
	default:
	}
	c.ended = err
	close(c.done)
}

// ending is what Send and Recv report once the session is over: why it ended
// if a pump failed, and io.EOF if it was simply closed.
func (c *StreamConn) ending() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended != nil {
		return c.ended
	}
	return io.EOF
}

// Recv allows the gateway to do a blocking wait on data coming from the reader.
func (c *StreamConn) Recv(ctx context.Context) (*Request, error) {
	select {
	case f := <-c.rCh:
		if f.err != nil {
			return nil, fmt.Errorf("recv: %w", f.err)
		}
		return f.req, nil
	case <-c.done:
		return nil, c.ending()
	case <-ctx.Done():
		return nil, fmt.Errorf("recv canceled (%v): %w", context.Cause(ctx), ctx.Err())
	}
}

// Send allows the gateway to do a blocking wait on sending data to the writer.
//
// It waits for the write itself, so nil means the peer has the bytes. An error
// means they may or may not have landed: a write already under way finishes
// whatever this call reports, and an expired context does not stop one that has
// not started. So giving up on a Send ends a session rather than being
// something to retry.
func (c *StreamConn) Send(ctx context.Context, msg *Response) error {
	ack := make(chan error, 1)
	select {
	case c.wCh <- responseFrame{resp: msg, ack: ack}:
	case <-c.done:
		return c.ending()
	case <-ctx.Done():
		return fmt.Errorf("send canceled (%v): %w", context.Cause(ctx), ctx.Err())
	}
	select {
	case err := <-ack:
		return err
	case <-c.done:
		return c.ending()
	case <-ctx.Done():
		return fmt.Errorf("send ack canceled (%v): %w", context.Cause(ctx), ctx.Err())
	}
}

// Gone closes once the client can send nothing more: it hung up, its stream
// broke, or it sent a frame this conn will not read past.
func (c *StreamConn) Gone() <-chan struct{} {
	return c.gone
}

// Close ends the session: it stops the pumps and closes the streams under them,
// and makes Send and Recv report the ending from then on. The caller that
// closes it gets the result of closing the streams, and every other caller
// gets io.EOF.
func (c *StreamConn) Close() error {
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return io.EOF
	default:
	}
	close(c.done)
	c.mu.Unlock()

	c.stop()
	return c.closeStream()
}
