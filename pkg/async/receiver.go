package async

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"path"
	"sync"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"golang.org/x/sync/errgroup"
)

const defaultSessionLifetime = 4 * time.Hour

// ReceiverOption configures a Receiver.
type ReceiverOption func(*receiverConfig)

type receiverConfig struct {
	mp              metric.MeterProvider
	httpClient      *http.Client
	name            string
	auditLog        *slog.Logger
	concurrency     int
	sessionLifetime time.Duration
}

// Receiver accepts request frames from a service inbox and supervises the
// ephemeral workers that own active HTTP sessions.
type Receiver struct {
	eq       *entroq.EntroQ
	upstream string
	cfg      receiverConfig

	handled       metric.Int64Counter
	forwardErrors metric.Int64Counter
	duration      metric.Float64Histogram
}

type sessionStart struct {
	inbox     string
	docNS     string
	session   string
	collectAt time.Time
}

// WithReceiverMeterProvider sets the OTel MeterProvider for the receiver.
// Defaults to a no-op provider if not set.
func WithReceiverMeterProvider(mp metric.MeterProvider) ReceiverOption {
	return func(c *receiverConfig) {
		c.mp = mp
	}
}

// WithReceiverHTTPClient sets the HTTP client used for forwarding requests.
// This allows for custom TLS configurations, timeouts, and connection pooling.
func WithReceiverHTTPClient(client *http.Client) ReceiverOption {
	return func(c *receiverConfig) {
		c.httpClient = client
	}
}

// WithReceiverName sets the service identity used in audit log entries as the
// "queue" field. Typically the service's own queue prefix (e.g. "payments/svc-b").
func WithReceiverName(name string) ReceiverOption {
	return func(c *receiverConfig) {
		c.name = name
	}
}

// WithReceiverAuditLogger enables structured audit logging. When set, a
// request_handled event is emitted after each task is forwarded to the upstream.
func WithReceiverAuditLogger(l *slog.Logger) ReceiverOption {
	return func(c *receiverConfig) {
		c.auditLog = l
	}
}

// WithReceiverConcurrency sets the number of workers accepting new sessions
// from the primary service inbox. Defaults to one.
func WithReceiverConcurrency(n int) ReceiverOption {
	return func(c *receiverConfig) {
		c.concurrency = n
	}
}

// WithReceiverSessionLifetime sets the maximum lifetime of one HTTP session.
// It also determines the initial garbage-collection deadline of the receiver's
// ephemeral inbox. Defaults to four hours.
func WithReceiverSessionLifetime(d time.Duration) ReceiverOption {
	return func(c *receiverConfig) {
		c.sessionLifetime = d
	}
}

// NewReceiver creates a receiver that forwards requests to upstream through
// the supplied EntroQ client. Run must be called to accept sessions.
func NewReceiver(eq *entroq.EntroQ, upstream string, opts ...ReceiverOption) *Receiver {
	cfg := &receiverConfig{
		mp: noop.NewMeterProvider(),
		httpClient: &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 32,
			},
		},
		concurrency:     1,
		sessionLifetime: defaultSessionLifetime,
	}
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
	return &Receiver{
		eq:            eq,
		upstream:      upstream,
		cfg:           *cfg,
		handled:       handled,
		forwardErrors: forwardErrors,
		duration:      duration,
	}
}

// Run claims initial request frames from inbox and supervises every resulting
// session until ctx is canceled or a worker encounters an unclassified error.
func (r *Receiver) Run(ctx context.Context, inbox string) error {
	if r.eq == nil {
		return fmt.Errorf("receiver run: nil EntroQ client")
	}
	if inbox == "" {
		return fmt.Errorf("receiver run: empty inbox")
	}
	if r.cfg.concurrency < 1 {
		return fmt.Errorf("receiver run: concurrency must be positive")
	}
	if r.cfg.sessionLifetime <= 0 {
		return fmt.Errorf("receiver run: session lifetime must be positive")
	}

	starts := make(chan sessionStart)
	g, gctx := errgroup.WithContext(ctx)

	// The supervisor is the only goroutine that adds session workers to the
	// group. starts is never closed: producers select on gctx, so shutdown
	// cannot race a send against channel closure.
	g.Go(func() error {
		for {
			select {
			case start := <-starts:
				g.Go(func() error {
					if err := r.runSession(gctx, start); err != nil {
						return fmt.Errorf("receiver session %q: %w", start.session, err)
					}
					return nil
				})
			case <-gctx.Done():
				return nil
			}
		}
	})

	bootstrap := worker.New(r.eq,
		worker.WithDoModify(r.bootstrapHandler(gctx, starts)),
		worker.WithMeterProvider[Envelope](r.cfg.mp),
	)
	for range r.cfg.concurrency {
		g.Go(func() error {
			if err := bootstrap.Run(gctx, worker.Watching(inbox)); err != nil {
				return fmt.Errorf("receiver bootstrap: %w", err)
			}
			return nil
		})
	}

	return g.Wait()
}

func (r *Receiver) bootstrapHandler(runCtx context.Context, starts chan<- sessionStart) worker.DoModifyRun[Envelope] {
	return func(_ context.Context, task *entroq.Task, env Envelope, _ []*entroq.Doc) (*worker.Result, error) {
		if env.Session == "" {
			return nil, worker.MoveErrorf("eqlink frame has no session")
		}
		if env.ReplyQueue == "" {
			return nil, worker.MoveErrorf("eqlink initial frame has no reply queue")
		}
		if env.Final {
			return nil, worker.MoveErrorf("eqlink initial frame is final")
		}

		prefix := path.Dir(task.Queue)
		collectAt := time.Now().Add(r.cfg.sessionLifetime)
		start := sessionStart{
			inbox:     sessionQueue(prefix, env.Session, collectAt, "request"),
			docNS:     connectionDocNamespace(prefix),
			session:   env.Session,
			collectAt: collectAt,
		}
		value, err := json.Marshal(env)
		if err != nil {
			return nil, fmt.Errorf("receiver bootstrap marshal frame: %w", err)
		}

		return worker.Modify(
			task.Delete(),
			entroq.InsertingInto(start.inbox, entroq.WithRawValue(value)),
			entroq.PuttingDocInto(start.docNS,
				entroq.WithIDKeys(env.Session, env.Session, ""),
				entroq.WithDocArrivalTimeBy(entroq.DefaultClaimDuration),
			),
		).OnSuccess(func(context.Context) error {
			select {
			case starts <- start:
				return nil
			case <-runCtx.Done():
				// The atomic handoff already committed, but shutdown won the
				// race with local session startup. The private task and doc have
				// finite claims and GC policy, so no local cleanup is safe or
				// necessary here.
				return nil
			}
		}), nil
	}
}

func (r *Receiver) runSession(ctx context.Context, start sessionStart) error {
	ctx, cancel := context.WithDeadline(ctx, start.collectAt)
	defer cancel()

	// Bootstrap inserted this doc as claimed by r.eq. Verify and extend that
	// claim before entering the worker. Without this check, a missing doc would
	// make the generic worker quarantine the private session task and then wait
	// silently on an inbox that can never receive another bootstrap frame.
	if _, err := r.eq.TryClaimDocByID(ctx, start.docNS, start.session, entroq.DefaultClaimDuration); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("claim connection doc: %w", err)
	}

	completed := make(chan struct{})
	var completeOnce sync.Once
	complete := func() {
		completeOnce.Do(func() {
			close(completed)
			cancel()
		})
	}

	sessionWorker := worker.New(r.eq,
		worker.WithTakeDocs(func(_ context.Context, _ *entroq.Task, env Envelope) ([]*entroq.DocClaim, error) {
			if env.Session != start.session {
				return nil, worker.FatalErrorf("session mismatch: frame %q, worker %q", env.Session, start.session)
			}
			return []*entroq.DocClaim{entroq.ClaimKey(start.docNS, start.session)}, nil
		}),
		worker.WithDoModify(r.sessionHandler(complete)),
		worker.WithMeterProvider[Envelope](r.cfg.mp),
	)
	if err := sessionWorker.Run(ctx, worker.Watching(start.inbox)); err != nil {
		return err
	}

	select {
	case <-completed:
		return nil
	default:
	}
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("worker exited before terminal frame committed")
}

func (r *Receiver) sessionHandler(complete func()) worker.DoModifyRun[Envelope] {
	return func(ctx context.Context, task *entroq.Task, env Envelope, docs []*entroq.Doc) (*worker.Result, error) {
		if len(docs) != 1 {
			return nil, worker.FatalErrorf("session %q claimed %d connection docs", env.Session, len(docs))
		}

		start := time.Now()
		defer func() {
			r.handled.Add(ctx, 1)
			r.duration.Record(ctx, time.Since(start).Seconds())
		}()

		resp, err := forward(ctx, r.cfg.httpClient, r.upstream, env)
		resp.Session = env.Session
		resp.Final = true
		if err != nil {
			log.Printf("receiver forward %s%s: %v", r.upstream, env.Path, err)
			r.forwardErrors.Add(ctx, 1)
		}
		if r.cfg.auditLog != nil {
			r.cfg.auditLog.LogAttrs(ctx, slog.LevelInfo, "request_handled",
				slog.String("queue", r.cfg.name),
				slog.String("response_queue", env.ReplyQueue),
				slog.String("method", env.Method),
				slog.String("path", env.Path),
				slog.Int("status_code", resp.StatusCode),
				slog.Float64("duration_s", time.Since(start).Seconds()),
			)
		}

		respValue, err := json.Marshal(resp)
		if err != nil {
			return nil, fmt.Errorf("receiver marshal response: %w", err)
		}

		return worker.Modify(
			task.Delete(),
			docs[0].Delete(),
			entroq.InsertingInto(env.ReplyQueue, entroq.WithRawValue(respValue)),
		).OnSuccess(func(context.Context) error {
			complete()
			return nil
		}), nil
	}
}

func sessionQueue(prefix, session string, collectAt time.Time, direction string) string {
	return path.Join(prefix,
		fmt.Sprintf("sess=%s;gc=%d", session, collectAt.Unix()),
		direction,
	)
}

func connectionDocNamespace(prefix string) string {
	return path.Join(prefix, "connections", "gc=")
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
		maps.Copy(req.Header, env.Headers)
		return req, nil
	}

	var lastErr error
	for attempt := range forwardMaxAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Response{
					FrameControl: FrameControl{Error: ctx.Err().Error()},
					StatusCode:   http.StatusGatewayTimeout,
				}, ctx.Err()
			case <-time.After(forwardDelay(attempt)):
			}
		}

		req, err := makeReq()
		if err != nil {
			return Response{
				FrameControl: FrameControl{Error: fmt.Sprintf("build request: %v", err)},
				StatusCode:   http.StatusInternalServerError,
			}, err
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
			return Response{
				FrameControl: FrameControl{Error: fmt.Sprintf("read response body: %v", err)},
				StatusCode:   http.StatusBadGateway,
			}, err
		}

		return Response{
			StatusCode: httpResp.StatusCode,
			Headers:    copyHeaders(httpResp.Header),
			Body:       body,
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
		FrameControl: FrameControl{
			Error: fmt.Sprintf("upstream unreachable after %d attempts: %v", forwardMaxAttempts, lastErr),
		},
		StatusCode: code,
	}, lastErr
}
