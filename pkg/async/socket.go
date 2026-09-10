package async

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"
)

// socketOpen supplies the metadata needed to open the upstream request. done
// is buffered so construction failure can always be reported even when the
// session is canceled concurrently.
type socketOpen struct {
	request Envelope
	done    chan error
}

// socketDelivery is one request-body segment sent from the request worker to
// the upstream socket owner. A successful result means the bytes reached the
// request pipe; it does not promise that the remote application consumed them.
type socketDelivery struct {
	event bodyEvent
	done  chan error
}

// socketEvent is one upstream response observation. HTTP read errors travel in
// the terminal event so the response worker can commit an error frame before
// ending the session. Errors in the pump machinery itself cancel the entire
// supervised session.
type socketEvent struct {
	statusCode  int
	headers     http.Header
	trailerKeys []string
	body        []byte
	trailers    http.Header
	end         bool
	err         error
}

// responseSocket owns both directions of one upstream HTTP request. Request
// workers deliver body bytes through deliveries while response workers consume
// events. Channels are bounded and never closed; every blocking channel
// operation also observes the session context.
type responseSocket struct {
	opens      chan socketOpen
	deliveries chan socketDelivery
	events     chan socketEvent
}

func newResponseSocket() *responseSocket {
	return &responseSocket{
		opens:      make(chan socketOpen),
		deliveries: make(chan socketDelivery),
		events:     make(chan socketEvent, 1),
	}
}

func (s *responseSocket) open(ctx context.Context, request Envelope) error {
	done := make(chan error, 1)
	select {
	case s.opens <- socketOpen{request: request, done: done}:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *responseSocket) write(ctx context.Context, event bodyEvent) error {
	done := make(chan error, 1)
	select {
	case s.deliveries <- socketDelivery{event: event, done: done}:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// nextBefore returns forced=true when deadline arrives before a response
// event. An already-buffered event wins at the exact boundary so ordinary data
// receives the first opportunity to piggyback a queue switch.
func (s *responseSocket) nextBefore(ctx context.Context, deadline time.Time) (event socketEvent, forced bool, err error) {
	select {
	case event := <-s.events:
		return event, false, nil
	default:
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case event := <-s.events:
		return event, false, nil
	case <-timer.C:
		select {
		case event := <-s.events:
			return event, false, nil
		default:
			return socketEvent{}, true, nil
		}
	case <-ctx.Done():
		return socketEvent{}, false, ctx.Err()
	}
}

func (s *responseSocket) send(ctx context.Context, event socketEvent) error {
	select {
	case s.events <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *responseSocket) run(ctx context.Context, client *http.Client, upstream string) error {
	var open socketOpen
	select {
	case open = <-s.opens:
	case <-ctx.Done():
		return nil
	}

	request, requestBody, err := newUpstreamRequest(ctx, upstream, open.request)
	if err != nil {
		open.done <- err
		response := responseForSocketError(open.request.Session, err)
		_ = s.send(ctx, socketEvent{
			statusCode: response.StatusCode,
			end:        true,
			err:        err,
		})
		return nil
	}
	open.done <- nil

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return s.writeRequest(gctx, request, requestBody)
	})
	g.Go(func() error {
		return s.readResponse(gctx, client, request)
	})
	return g.Wait()
}

func newUpstreamRequest(ctx context.Context, upstream string, env Envelope) (*http.Request, *io.PipeWriter, error) {
	reader, writer := io.Pipe()
	request, err := http.NewRequestWithContext(ctx, env.Method, upstream+env.Path, reader)
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, nil, fmt.Errorf("build upstream request: %w", err)
	}
	maps.Copy(request.Header, env.Headers)
	request.ContentLength = env.ContentLength
	request.Trailer = headerWithKeys(env.TrailerKeys)
	if env.ProtocolMajor >= 2 {
		request.Proto = "HTTP/2.0"
		request.ProtoMajor = 2
		request.ProtoMinor = 0
	}
	return request, writer, nil
}

func (s *responseSocket) writeRequest(ctx context.Context, request *http.Request, body *io.PipeWriter) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = body.CloseWithError(context.Cause(ctx))
		case <-done:
		}
	}()

	for {
		var delivery socketDelivery
		select {
		case delivery = <-s.deliveries:
		case <-ctx.Done():
			return nil
		}

		var writeErr error
		if len(delivery.event.body) > 0 {
			_, writeErr = body.Write(delivery.event.body)
		}
		if writeErr != nil {
			_ = body.CloseWithError(writeErr)
			delivery.done <- writeErr
			return nil
		}
		if !delivery.event.end {
			delivery.done <- nil
			continue
		}

		maps.Copy(request.Trailer, delivery.event.trailers)
		closeErr := body.CloseWithError(delivery.event.err)
		delivery.done <- closeErr
		return nil
	}
}

func (s *responseSocket) readResponse(ctx context.Context, client *http.Client, request *http.Request) error {
	response, err := client.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			if closeErr := response.Body.Close(); closeErr != nil {
				log.Printf("close failed upstream response: %v", closeErr)
			}
		}
		if sendErr := s.send(ctx, socketEvent{
			statusCode: responseForSocketError("", err).StatusCode,
			end:        true,
			err:        err,
		}); sendErr != nil {
			return nil
		}
		return nil
	}
	readDone := make(chan struct{})
	defer close(readDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = response.Body.Close()
		case <-readDone:
		}
	}()

	if err := s.send(ctx, socketEvent{
		statusCode:  response.StatusCode,
		headers:     copyHeaders(response.Header),
		trailerKeys: trailerKeys(response.Trailer),
	}); err != nil {
		_ = response.Body.Close()
		return nil
	}

	buffer := make([]byte, streamReadBufferSize)
	emptyReads := 0
	for {
		n, readErr := response.Body.Read(buffer)
		if n == 0 && readErr == nil {
			emptyReads++
			if emptyReads < 100 {
				continue
			}
			readErr = io.ErrNoProgress
		} else {
			emptyReads = 0
		}

		event := socketEvent{body: append([]byte(nil), buffer[:n]...)}
		if readErr != nil {
			closeErr := response.Body.Close()
			if errors.Is(readErr, io.EOF) {
				readErr = nil
			}
			event.end = true
			event.err = errors.Join(readErr, closeErr)
			event.trailers = copyHeaders(response.Trailer)
		}
		if err := s.send(ctx, event); err != nil {
			_ = response.Body.Close()
			return nil
		}
		if event.end {
			return nil
		}
	}
}

func responseForSocketError(session string, err error) Response {
	status := http.StatusBadGateway
	var netErr net.Error
	if (errors.As(err, &netErr) && netErr.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	return Response{
		FrameControl: FrameControl{
			Session: session,
			Final:   true,
			Error:   err.Error(),
		},
		StatusCode: status,
	}
}
