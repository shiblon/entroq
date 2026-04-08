package async

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// ReceiverOption configures a ReceiverHandler.
type ReceiverOption func(*receiverConfig)

type receiverConfig struct {
	mp metric.MeterProvider
}

// WithReceiverMeterProvider sets the OTel MeterProvider for the receiver.
// Defaults to a no-op provider if not set.
func WithReceiverMeterProvider(mp metric.MeterProvider) ReceiverOption {
	return func(c *receiverConfig) {
		c.mp = mp
	}
}

// ReceiverHandler returns a worker.DoModifyRun that claims Envelope tasks,
// forwards them as HTTP requests to upstream, and enqueues a Response onto
// the envelope's ResponseQueue. Errors from the upstream are packed into the
// Response rather than returned, so the sender always unblocks with a result.
//
// Use with a standard worker.Worker:
//
//	worker.New(eq,
//	    worker.WithQueue("svc-a"),
//	    worker.WithDoModify(async.ReceiverHandler("http://localhost:8000")),
//	).Run(ctx)
func ReceiverHandler(upstream string, opts ...ReceiverOption) worker.DoModifyRun[Envelope] {
	cfg := &receiverConfig{mp: noop.NewMeterProvider()}
	for _, o := range opts {
		o(cfg)
	}

	m := cfg.mp.Meter("entroq/async/receiver")
	handled, _ := m.Int64Counter("receiver.handled_total",
		metric.WithDescription("Total number of tasks handled by the receiver."),
	)
	forwardErrors, _ := m.Int64Counter("receiver.forward_errors_total",
		metric.WithDescription("Total number of upstream forwarding errors encountered by the receiver."),
	)
	duration, _ := m.Float64Histogram("receiver.duration_seconds",
		metric.WithDescription("Task handling duration in seconds."),
		metric.WithUnit("s"),
	)

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 32,
		},
	}
	return func(ctx context.Context, task *entroq.Task, env Envelope) ([]entroq.ModifyArg, error) {
		start := time.Now()
		defer func() {
			handled.Add(ctx, 1)
			duration.Record(ctx, time.Since(start).Seconds())
		}()

		resp, err := forward(ctx, client, upstream, env)
		if err != nil {
			log.Printf("receiver forward %s%s: %v", upstream, env.Path, err)
			forwardErrors.Add(ctx, 1)
		}

		respValue, err := json.Marshal(resp)
		if err != nil {
			return nil, fmt.Errorf("receiver marshal response: %w", err)
		}

		return []entroq.ModifyArg{
			task.Delete(),
			entroq.InsertingInto(env.ResponseQueue, entroq.WithRawValue(respValue)),
		}, nil
	}
}

const (
	forwardMaxAttempts = 3
	forwardBaseDelay   = 500 * time.Millisecond
	forwardMaxDelay    = 5 * time.Second
)

// forwardDelay returns the backoff duration for the given attempt number,
// capped at forwardMaxDelay.
func forwardDelay(attempt int) time.Duration {
	d := forwardBaseDelay * (1 << (attempt - 1))
	if d > forwardMaxDelay {
		return forwardMaxDelay
	}
	return d
}

// forward sends the envelope as an HTTP request to upstream and returns a
// Response and any error. Network errors (upstream unreachable) are retried
// with exponential backoff up to forwardMaxAttempts times. HTTP responses
// (including 4xx/5xx) are returned immediately without retry -- the upstream
// answered, and that answer goes back to the caller. The error is returned
// separately for logging at the call site.
func forward(ctx context.Context, client *http.Client, upstream string, env Envelope) (Response, error) {
	// Build request is not retried -- a failure here is a programming error.
	// We do recreate it on each attempt since the body reader is consumed.
	makeReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, env.Method, upstream+env.Path, bytes.NewReader(env.Body))
		if err != nil {
			return nil, fmt.Errorf("new request in forward: %w", err)
		}
		for k, v := range env.Headers {
			req.Header.Set(k, v)
		}
		return req, nil
	}

	var lastErr error
	for attempt := range forwardMaxAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Response{StatusCode: http.StatusGatewayTimeout, Error: ctx.Err().Error()}, ctx.Err()
			case <-time.After(forwardDelay(attempt)):
			}
		}

		req, err := makeReq()
		if err != nil {
			return Response{StatusCode: http.StatusInternalServerError, Error: fmt.Sprintf("build request: %v", err)}, err
		}

		httpResp, err := client.Do(req)
		if err != nil {
			log.Printf("client.Do failure: %v", err)
			lastErr = err
			continue
		}

		body, err := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		if err != nil {
			return Response{StatusCode: http.StatusBadGateway, Error: fmt.Sprintf("read response body: %v", err)}, err
		}

		headers := make(map[string]string, len(httpResp.Header))
		for k := range httpResp.Header {
			headers[k] = httpResp.Header.Get(k)
		}

		return Response{
			StatusCode: httpResp.StatusCode,
			Headers:    headers,
			Body:       json.RawMessage(body),
		}, nil
	}

	// All attempts exhausted -- upstream unreachable.
	code := http.StatusBadGateway
	var netErr net.Error
	if errors.As(lastErr, &netErr) && netErr.Timeout() {
		code = http.StatusGatewayTimeout
	} else if errors.Is(lastErr, context.DeadlineExceeded) {
		code = http.StatusGatewayTimeout
	}
	return Response{
		StatusCode: code,
		Error:      fmt.Sprintf("upstream unreachable after %d attempts: %v", forwardMaxAttempts, lastErr),
	}, lastErr
}
