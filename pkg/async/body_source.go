package async

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

const streamReadBufferSize = 1 << 20

// bodyEvent is one arbitrary byte segment observed at an HTTP body boundary.
// end reports EOF or a read failure; callers must still process body before
// handling err because io.Reader permits data and a terminal error together.
type bodyEvent struct {
	body     []byte
	trailers http.Header
	end      bool
	err      error
}

// bodySource owns one local request body. Its single buffered event and the
// event held by a blocked send bound read-ahead while keeping socket reads out
// of worker handlers. The body is closed on cancellation so a blocked Read is
// released; channels are never closed, and every operation selects on ctx.
type bodySource struct {
	body     io.ReadCloser
	trailers http.Header
	events   chan bodyEvent
}

func newBodySource(body io.ReadCloser, trailers http.Header) *bodySource {
	return &bodySource{
		body:     body,
		trailers: trailers,
		events:   make(chan bodyEvent, 1),
	}
}

// run is the sole reader of the local request body. It publishes bounded body
// events until EOF or a read failure; canceling ctx closes the body to release
// a blocked Read, while normal completion stops that cancellation callback.
func (s *bodySource) run(ctx context.Context) error {
	stopClose := context.AfterFunc(ctx, func() { s.body.Close() })
	defer stopClose()

	buffer := make([]byte, streamReadBufferSize)
	emptyReads := 0
	for {
		n, readErr := s.body.Read(buffer)
		if n == 0 && readErr == nil {
			emptyReads++
			if emptyReads < 100 {
				continue
			}
			readErr = io.ErrNoProgress
		} else {
			emptyReads = 0
		}

		event := bodyEvent{body: append([]byte(nil), buffer[:n]...)}
		if readErr != nil {
			event.end = true
			if !errors.Is(readErr, io.EOF) {
				event.err = readErr
			}
			event.trailers = copyHeaders(s.trailers)
		}
		if err := s.send(ctx, event); err != nil {
			return nil
		}
		if event.end {
			return nil
		}
	}
}

// nextBefore returns the next body event, or forced=true when deadline wins.
func (s *bodySource) nextBefore(ctx context.Context, deadline time.Time) (event bodyEvent, forced bool, err error) {
	return receiveBefore(ctx, s.events, deadline)
}

// receiveBefore waits for an event until an absolute deadline. When both the
// event and deadline are ready, ordinary select semantics choose the result.
func receiveBefore[T any](ctx context.Context, events <-chan T, deadline time.Time) (event T, forced bool, err error) {
	select {
	case event = <-events:
		return event, false, nil
	case <-time.After(time.Until(deadline)):
		return event, true, nil
	case <-ctx.Done():
		return event, false, ctx.Err()
	}
}

func (s *bodySource) send(ctx context.Context, event bodyEvent) error {
	select {
	case s.events <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
