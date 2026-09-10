package async

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"time"
)

const (
	responseReadBufferSize = 1 << 20
	forwardMaxAttempts     = 3
	forwardBaseDelay       = 500 * time.Millisecond
	forwardMaxDelay        = 5 * time.Second
)

// socketDelivery is a worker-to-socket command. done is buffered so the socket
// pump can always report the result even if the session is canceled at the
// same moment; the worker still explicitly receives that result before it
// proceeds.
type socketDelivery struct {
	request Envelope
	done    chan error
}

// socketEvent is one socket-to-worker observation. HTTP read errors travel in
// the event so the worker can send a terminal error frame before ending the
// session. Errors in the pump machinery itself are returned from run and
// cancel the whole supervised session.
type socketEvent struct {
	statusCode int
	headers    http.Header
	body       []byte
	final      bool
	err        error
}

// responseSocket owns the upstream HTTP request and response body. Its
// channels are bounded and never closed; every send and receive also selects
// on the session context, avoiding send-on-close and abandoned-send races.
type responseSocket struct {
	deliveries chan socketDelivery
	events     chan socketEvent
}

func newResponseSocket() *responseSocket {
	return &responseSocket{
		deliveries: make(chan socketDelivery),
		events:     make(chan socketEvent, 1),
	}
}

func (s *responseSocket) open(ctx context.Context, request Envelope) error {
	done := make(chan error, 1)
	select {
	case s.deliveries <- socketDelivery{request: request, done: done}:
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

func (s *responseSocket) next(ctx context.Context) (socketEvent, error) {
	select {
	case event := <-s.events:
		return event, nil
	case <-ctx.Done():
		return socketEvent{}, ctx.Err()
	}
}

// nextBefore returns forced=true when deadline arrives before a socket event.
// An event that is already buffered wins over the watchdog, including at the
// exact boundary, so ordinary data gets the first opportunity to piggyback a
// queue switch.
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
	var delivery socketDelivery
	select {
	case delivery = <-s.deliveries:
	case <-ctx.Done():
		return nil
	}

	response, err := openUpstreamResponse(ctx, client, upstream, delivery.request)
	if err != nil {
		delivery.done <- err
		<-ctx.Done()
		return nil
	}
	delivery.done <- nil

	if err := s.send(ctx, socketEvent{
		statusCode: response.StatusCode,
		headers:    copyHeaders(response.Header),
	}); err != nil {
		if closeErr := response.Body.Close(); closeErr != nil {
			log.Printf("close canceled upstream response: %v", closeErr)
		}
		return nil
	}

	buffer := make([]byte, responseReadBufferSize)
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

		body := append([]byte(nil), buffer[:n]...)
		if readErr == nil {
			if err := s.send(ctx, socketEvent{body: body}); err != nil {
				if closeErr := response.Body.Close(); closeErr != nil {
					log.Printf("close canceled upstream response: %v", closeErr)
				}
				return nil
			}
			continue
		}

		closeErr := response.Body.Close()
		if errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		terminalErr := errors.Join(readErr, closeErr)
		if err := s.send(ctx, socketEvent{body: body, final: true, err: terminalErr}); err != nil {
			return nil
		}
		return nil
	}
}

func openUpstreamResponse(ctx context.Context, client *http.Client, upstream string, env Envelope) (*http.Response, error) {
	var lastErr error
	for attempt := range forwardMaxAttempts {
		if attempt > 0 {
			timer := time.NewTimer(forwardDelay(attempt))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			}
		}

		request, err := http.NewRequestWithContext(ctx, env.Method, upstream+env.Path, bytes.NewReader(env.Body))
		if err != nil {
			return nil, fmt.Errorf("build upstream request: %w", err)
		}
		maps.Copy(request.Header, env.Headers)

		response, err := client.Do(request)
		if err == nil {
			return response, nil
		}
		if response != nil && response.Body != nil {
			if closeErr := response.Body.Close(); closeErr != nil {
				log.Printf("close failed upstream response: %v", closeErr)
			}
		}
		log.Printf("client.Do failure: %v", err)
		lastErr = err
	}
	return nil, fmt.Errorf("upstream unreachable after %d attempts: %w", forwardMaxAttempts, lastErr)
}

func forwardDelay(attempt int) time.Duration {
	delay := forwardBaseDelay * (1 << (attempt - 1))
	if delay > forwardMaxDelay {
		return forwardMaxDelay
	}
	return delay
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
