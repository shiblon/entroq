package workgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// RWConn is a reader-writer connection that can be used with a work Gateway.
// Anything that satisfies ReadCloser and WriteCloser can be used as transport
// channels for the gateway.
type RWConn struct {
	sync.Mutex

	dec *json.Decoder
	enc *json.Encoder

	r io.ReadCloser
	w io.WriteCloser

	done chan bool
}

// NewRWConn creates a new read-write connection for a Gateway.
// The connection reads from the reader and forwards it to the gateway. The
// gateway responses are forwarded to the writer. Send is from the gateway's
// perspective, as is Recv.
func NewRWConn(r io.ReadCloser, w io.WriteCloser) *RWConn {
	return &RWConn{
		r:    r,
		w:    w,
		dec:  json.NewDecoder(r),
		enc:  json.NewEncoder(w),
		done: make(chan bool),
	}
}

// Recv allows the gateway to do a blocking wait on data coming from the reader.
func (c *RWConn) Recv(ctx context.Context) (*Request, error) {
	select {
	case <-c.done:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, fmt.Errorf("recv canceled (%v): %w", context.Cause(ctx), ctx.Err())
	default:
	}
	msg := new(Request)
	if err := c.dec.Decode(msg); err != nil {
		return nil, fmt.Errorf("recv decode: %w", err)
	}
	return msg, nil
}

// Send allows the gateway to do a blocking wait on sending data to the writer.
func (c *RWConn) Send(ctx context.Context, msg *Response) error {
	select {
	case <-c.done:
		return io.EOF
	case <-ctx.Done():
		return fmt.Errorf("send canceled (%v): %w", context.Cause(ctx), ctx.Err())
	default:
	}
	if err := c.enc.Encode(msg); err != nil {
		return fmt.Errorf("send encode: %w", err)
	}
	return nil
}

// Close closes the readers and writers and causes Send and Recv to return EOF errors.
func (c *RWConn) Close() error {
	c.Lock()
	defer c.Unlock()
	select {
	case <-c.done:
		return io.EOF
	default:
	}
	close(c.done)
	c.w.Close()
	c.r.Close()
	return nil
}
