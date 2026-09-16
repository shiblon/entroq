package async

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"path"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"golang.org/x/sync/errgroup"
)

// ReceiverOption configures a Receiver.
type ReceiverOption func(*receiverConfig)

type receiverConfig struct {
	mp          metric.MeterProvider
	httpClient  *http.Client
	name        string
	auditLog    *slog.Logger
	concurrency int
	laneTiming  laneTiming
	heartbeat   heartbeatTiming
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
	requestData *receiveLane
	responseAck *receiveLane
	docNS       string
	session     string
	method      string
	path        string
	startedAt   time.Time
}

// WithReceiverMeterProvider sets the OTel MeterProvider for the receiver.
// Defaults to a no-op provider if not set.
func WithReceiverMeterProvider(mp metric.MeterProvider) ReceiverOption {
	return func(c *receiverConfig) { c.mp = mp }
}

// WithReceiverHTTPClient sets the HTTP client used for forwarding requests.
// A custom client is responsible for selecting HTTP/2 when ProtocolMajor is 2.
func WithReceiverHTTPClient(client *http.Client) ReceiverOption {
	return func(c *receiverConfig) { c.httpClient = client }
}

// WithReceiverTLSConfig sets TLS authentication for upstream HTTP connections
// while retaining EQLink's HTTP/1.1, HTTP/2, and h2c protocol selection.
func WithReceiverTLSConfig(config *tls.Config) ReceiverOption {
	return func(c *receiverConfig) { c.httpClient = newProtocolHTTPClient(config) }
}

// WithReceiverName sets the service identity used in audit log entries as the
// "queue" field. Typically the service's own queue prefix.
func WithReceiverName(name string) ReceiverOption {
	return func(c *receiverConfig) { c.name = name }
}

// WithReceiverAuditLogger enables structured audit logging. When set, one
// request_handled event is emitted after an HTTP session completes.
func WithReceiverAuditLogger(l *slog.Logger) ReceiverOption {
	return func(c *receiverConfig) { c.auditLog = l }
}

// WithReceiverConcurrency sets the number of workers accepting new sessions
// from the primary service inbox. Active sessions run in independent workers
// after bootstrap and do not consume these slots. Defaults to one.
func WithReceiverConcurrency(n int) ReceiverOption {
	return func(c *receiverConfig) { c.concurrency = n }
}

// WithReceiverRequestTimeout sets the maximum silence from the peer before an
// active receiver session is abandoned. An empty heartbeat is sent at
// one-third of this duration. Defaults to three minutes.
func WithReceiverRequestTimeout(d time.Duration) ReceiverOption {
	return func(c *receiverConfig) { c.heartbeat = heartbeatTimingForTimeout(d) }
}

// NewReceiver creates a receiver that forwards requests to upstream through
// the supplied EntroQ client. Run must be called to accept sessions.
func NewReceiver(eq *entroq.EntroQ, upstream string, opts ...ReceiverOption) *Receiver {
	cfg := &receiverConfig{
		mp:          noop.NewMeterProvider(),
		httpClient:  newProtocolHTTPClient(nil),
		concurrency: 1,
		laneTiming:  defaultLaneTiming,
		heartbeat:   defaultHeartbeatTiming,
	}
	for _, option := range opts {
		option(cfg)
	}

	meter := cfg.mp.Meter("entroq/async/receiver")
	handled, _ := meter.Int64Counter("receiver.handled_total",
		metric.WithDescription("Total number of HTTP sessions handled by the receiver."),
	)
	forwardErrors, _ := meter.Int64Counter("receiver.forward_errors_total",
		metric.WithDescription("Total number of upstream forwarding errors encountered by the receiver."),
	)
	duration, _ := meter.Float64Histogram("receiver.duration_seconds",
		metric.WithDescription("HTTP session duration in seconds."),
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
	if err := r.cfg.laneTiming.validate(); err != nil {
		return fmt.Errorf("receiver run: %w", err)
	}
	if err := r.cfg.heartbeat.validate(); err != nil {
		return fmt.Errorf("receiver run: %w", err)
	}

	starts := make(chan sessionStart)
	g, gctx := errgroup.WithContext(ctx)
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
		if env.ReplyQueue == "" || env.ResponseQueue == "" {
			return nil, worker.MoveErrorf("eqlink initial frame has incomplete lane queues")
		}
		if env.Final || env.Error != "" {
			return nil, worker.MoveErrorf("eqlink initial frame is terminal")
		}
		if env.Method == "" {
			return nil, worker.MoveErrorf("eqlink initial frame has no HTTP method")
		}
		for name, queue := range map[string]string{"request ACK": env.ReplyQueue, "response data": env.ResponseQueue} {
			var peer peerLane
			if _, err := peer.observe(queue); err != nil {
				return nil, worker.MoveErrorf("eqlink initial %s queue: %v", name, err)
			}
		}

		prefix := path.Dir(task.Queue)
		startedAt := time.Now()
		start := sessionStart{
			requestData: newReceiveLane(prefix, env.Session, "request-data", startedAt, r.cfg.laneTiming),
			responseAck: newReceiveLane(prefix, env.Session, "response-ack", startedAt, r.cfg.laneTiming),
			docNS:       connectionDocNamespace(prefix),
			session:     env.Session,
			method:      env.Method,
			path:        env.Path,
			startedAt:   startedAt,
		}
		envValue, err := json.Marshal(env)
		if err != nil {
			return nil, fmt.Errorf("receiver bootstrap marshal request: %w", err)
		}
		seedValue, err := json.Marshal(Response{FrameControl: FrameControl{
			Session:    env.Session,
			ReplyQueue: env.ResponseQueue,
		}})
		if err != nil {
			return nil, fmt.Errorf("receiver bootstrap marshal response seed: %w", err)
		}

		return worker.Modify(
			task.Delete(),
			entroq.InsertingInto(start.requestData.queue, entroq.WithRawValue(envValue)),
			entroq.InsertingInto(start.responseAck.queue, entroq.WithRawValue(seedValue)),
			entroq.PuttingDocInto(start.docNS,
				entroq.WithIDKeys(env.Session, env.Session, ""),
				entroq.WithDocArrivalTimeBy(entroq.DefaultClaimDuration),
			),
		).OnSuccess(func(context.Context) error {
			select {
			case starts <- start:
				return nil
			case <-runCtx.Done():
				return nil
			}
		}), nil
	}
}

func (r *Receiver) runSession(ctx context.Context, start sessionStart) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if _, err := r.eq.TryClaimDocByID(ctx, start.docNS, start.session, entroq.DefaultClaimDuration); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("claim connection doc: %w", err)
	}

	socket := newResponseSocket()
	state := &receiverSessionState{
		session:       start.session,
		requestLanes:  sessionLanes{local: start.requestData},
		responseLanes: sessionLanes{local: start.responseAck},
		liveness:      newPeerLiveness(r.cfg.heartbeat),
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		if err := socket.run(gctx, r.cfg.httpClient, r.upstream); err != nil {
			return fmt.Errorf("upstream socket: %w", err)
		}
		return nil
	})
	g.Go(func() error { return r.runRequestWorkers(gctx, state, socket, cancel) })
	g.Go(func() error { return r.runResponseWorkers(gctx, start, state, socket, cancel) })
	g.Go(func() error {
		return state.liveness.run(gctx, func() {
			log.Printf("receiver session %q peer liveness timeout", start.session)
			cancel()
		})
	})
	if err := g.Wait(); err != nil {
		return err
	}

	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("session workers exited before terminal response committed")
}

type receiverSessionState struct {
	session       string
	requestLanes  sessionLanes
	responseLanes sessionLanes
	liveness      *peerLiveness

	opened                 bool
	requestCompleted       bool
	responseCompleted      bool
	requestWorkerSwitched  bool
	responseWorkerSwitched bool
	requestWorkerCancel    context.CancelFunc
	responseWorkerCancel   context.CancelFunc
	statusCode             int
}

func (r *Receiver) runRequestWorkers(ctx context.Context, state *receiverSessionState, socket *responseSocket, complete func()) error {
	for {
		workerCtx, stopWorker := context.WithDeadline(ctx, state.requestLanes.local.collectAt)
		state.requestWorkerCancel = stopWorker
		state.requestWorkerSwitched = false
		requestWorker := worker.New(r.eq,
			worker.WithDoModify(r.requestHandler(state, socket)),
			worker.WithMeterProvider[Envelope](r.cfg.mp),
		)
		err := requestWorker.Run(workerCtx, worker.Watching(state.requestLanes.local.queue))
		workerErr := workerCtx.Err()
		stopWorker()
		if err != nil {
			return fmt.Errorf("request data worker: %w", err)
		}
		if state.requestCompleted || ctx.Err() != nil {
			return nil
		}
		if errors.Is(workerErr, context.DeadlineExceeded) {
			log.Printf("receiver request lane %q expired; abandoning the upstream socket", state.requestLanes.local.queue)
			complete()
			return nil
		}
		if !state.requestWorkerSwitched {
			return fmt.Errorf("request data worker exited without completing or switching queues")
		}
	}
}

func (r *Receiver) requestHandler(state *receiverSessionState, socket *responseSocket) worker.DoModifyRun[Envelope] {
	return func(ctx context.Context, task *entroq.Task, env Envelope, _ []*entroq.Doc) (*worker.Result, error) {
		if env.Session != state.session {
			return nil, worker.FatalErrorf("request session mismatch: got %q, want %q", env.Session, state.session)
		}
		if env.Final && env.ReplyQueue != "" {
			return nil, worker.FatalErrorf("session %q terminal request has an ACK queue", env.Session)
		}
		if !env.Final && env.ReplyQueue == "" {
			return nil, worker.FatalErrorf("session %q request data has no ACK queue", env.Session)
		}
		if env.Error != "" && !env.Final {
			return nil, worker.FatalErrorf("session %q has a nonterminal request error", env.Session)
		}
		state.liveness.observed()

		if !state.opened {
			if env.Method == "" || env.ResponseQueue == "" {
				return nil, worker.FatalErrorf("session %q initial request metadata is incomplete", env.Session)
			}
			if err := socket.open(ctx, env); err != nil {
				ack := Envelope{FrameControl: FrameControl{Session: env.Session, Final: true, Error: err.Error()}}
				return worker.Modify(
					task.Delete(),
					entroq.InsertingInto(env.ReplyQueue, entroq.WithValue(ack)),
				).OnSuccess(func(context.Context) error {
					state.requestCompleted = true
					state.requestWorkerCancel()
					return nil
				}), nil
			}
			state.opened = true
		} else if hasRequestMetadata(env) {
			return nil, worker.FatalErrorf("session %q received repeated request metadata", env.Session)
		}

		reciprocate := false
		if !env.Final {
			var err error
			reciprocate, err = state.requestLanes.observePeer(env.ReplyQueue)
			if err != nil {
				return nil, worker.FatalErrorf("session %q request lane: %v", env.Session, err)
			}
		}

		consumedBody := len(env.Body) > 0 || env.Final
		if consumedBody {
			event := bodyEvent{body: env.Body, trailers: env.Trailers, end: env.Final}
			if env.Error != "" {
				event.err = errors.New(env.Error)
			}
			if err := socket.write(ctx, event); err != nil {
				mods := []entroq.ModifyArg{task.Delete()}
				if env.ReplyQueue != "" {
					mods = append(mods, entroq.InsertingInto(env.ReplyQueue, entroq.WithValue(Envelope{FrameControl: FrameControl{
						Session: env.Session,
						Final:   true,
						Error:   err.Error(),
					}})))
				}
				return worker.Modify(mods...).OnDependency(func(_ context.Context, depErr *entroq.DependencyError) error {
					return worker.FatalErrorf("session %q request failure commit lost dependency: %v", env.Session, depErr)
				}).OnSuccess(func(context.Context) error {
					state.requestCompleted = true
					state.requestWorkerCancel()
					return nil
				}), nil
			}
		}

		mods := []entroq.ModifyArg{task.Delete()}
		localSwitched := false
		if !env.Final {
			switch {
			case reciprocate:
				state.requestLanes.reciprocateSwitch(time.Now())
				localSwitched = true
			case state.requestLanes.local.shouldPiggyback(time.Now()):
				state.requestLanes.initiateSwitch(time.Now())
				localSwitched = true
			}
			mods = append(mods, entroq.InsertingInto(env.ReplyQueue, entroq.WithValue(Envelope{FrameControl: FrameControl{
				Session:    env.Session,
				ReplyQueue: state.requestLanes.local.queue,
			}})))
		}

		result := worker.Modify(mods...)
		if consumedBody {
			result = result.OnDependency(func(_ context.Context, err *entroq.DependencyError) error {
				return worker.FatalErrorf("session %q request commit lost dependency after socket write: %v", env.Session, err)
			})
		}
		return result.OnSuccess(func(context.Context) error {
			if env.Final {
				state.requestCompleted = true
				state.requestWorkerCancel()
			} else if localSwitched {
				state.requestWorkerSwitched = true
				state.requestWorkerCancel()
			}
			return nil
		}), nil
	}
}

func (r *Receiver) runResponseWorkers(ctx context.Context, start sessionStart, state *receiverSessionState, socket *responseSocket, complete func()) error {
	for {
		workerCtx, stopWorker := context.WithDeadline(ctx, state.responseLanes.local.collectAt)
		state.responseWorkerCancel = stopWorker
		state.responseWorkerSwitched = false
		responseWorker := worker.New(r.eq,
			// Only this lane touches the singleton connection doc. Letting both
			// concurrent workers claim it under the same claimant ID would remove
			// mutual exclusion and create version races between doc renewals.
			worker.WithTakeDocs(func(_ context.Context, _ *entroq.Task, ack Response) ([]*entroq.DocClaim, error) {
				if ack.Session != start.session {
					return nil, worker.FatalErrorf("session mismatch: ACK %q, worker %q", ack.Session, start.session)
				}
				return []*entroq.DocClaim{entroq.ClaimKey(start.docNS, start.session)}, nil
			}),
			worker.WithDoModify(r.responseHandler(start, state, socket, complete)),
			worker.WithMeterProvider[Response](r.cfg.mp),
		)
		err := responseWorker.Run(workerCtx, worker.Watching(state.responseLanes.local.queue))
		workerErr := workerCtx.Err()
		stopWorker()
		if err != nil {
			return fmt.Errorf("response ACK worker: %w", err)
		}
		if state.responseCompleted || ctx.Err() != nil {
			return nil
		}
		if errors.Is(workerErr, context.DeadlineExceeded) {
			log.Printf("receiver response lane %q expired; abandoning the upstream socket", state.responseLanes.local.queue)
			state.responseCompleted = true
			complete()
			return nil
		}
		if !state.responseWorkerSwitched {
			return fmt.Errorf("response ACK worker exited without completing or switching queues")
		}
	}
}

func (r *Receiver) responseHandler(start sessionStart, state *receiverSessionState, socket *responseSocket, complete func()) worker.DoModifyRun[Response] {
	return func(ctx context.Context, task *entroq.Task, ack Response, docs []*entroq.Doc) (*worker.Result, error) {
		if len(docs) != 1 {
			return nil, worker.FatalErrorf("session %q claimed %d connection docs", ack.Session, len(docs))
		}
		if !responseDataEmpty(ack) {
			return nil, worker.FatalErrorf("session %q received response data where an ACK was expected", ack.Session)
		}
		state.liveness.observed()
		if ack.Final {
			if ack.Error == "" || ack.ReplyQueue != "" {
				return nil, worker.FatalErrorf("session %q received invalid terminal response ACK", ack.Session)
			}
			return worker.Modify(task.Delete(), docs[0].Delete()).OnSuccess(func(context.Context) error {
				state.responseCompleted = true
				complete()
				return nil
			}), nil
		}
		if ack.Error != "" || ack.ReplyQueue == "" {
			return nil, worker.FatalErrorf("session %q received invalid response ACK", ack.Session)
		}

		reciprocate, err := state.responseLanes.observePeer(ack.ReplyQueue)
		if err != nil {
			return nil, worker.FatalErrorf("session %q response lane: %v", ack.Session, err)
		}

		var (
			resp          Response
			localSwitched bool
			consumedBody  bool
		)
		if reciprocate {
			state.responseLanes.reciprocateSwitch(time.Now())
			localSwitched = true
			resp.FrameControl = FrameControl{Session: ack.Session, ReplyQueue: state.responseLanes.local.queue}
		} else {
			forceAt := state.responseLanes.peer.forceAt(r.cfg.laneTiming)
			heartbeatAt := time.Now().Add(r.cfg.heartbeat.after)
			event, idle, err := socket.nextBefore(ctx, earlier(forceAt, heartbeatAt))
			if err != nil {
				return nil, err
			}
			if idle {
				if !heartbeatAt.Before(forceAt) {
					state.responseLanes.initiateSwitch(time.Now())
					localSwitched = true
				}
				resp.FrameControl = FrameControl{Session: ack.Session, ReplyQueue: state.responseLanes.local.queue}
			} else {
				consumedBody = true
				resp = Response{
					FrameControl: FrameControl{
						Session: ack.Session,
						Final:   event.end,
					},
					StatusCode:  event.statusCode,
					Headers:     event.headers,
					TrailerKeys: event.trailerKeys,
					Body:        event.body,
					Trailers:    event.trailers,
				}
				if event.statusCode != 0 {
					state.statusCode = event.statusCode
				}
				if event.err != nil {
					resp.Error = event.err.Error()
					log.Printf("receiver read %s%s: %v", r.upstream, start.path, event.err)
					r.forwardErrors.Add(ctx, 1)
				}
				if !resp.Final {
					if state.responseLanes.local.shouldPiggyback(time.Now()) {
						state.responseLanes.initiateSwitch(time.Now())
						localSwitched = true
					}
					resp.ReplyQueue = state.responseLanes.local.queue
				}
			}
		}

		value, err := json.Marshal(resp)
		if err != nil {
			return nil, fmt.Errorf("receiver marshal response: %w", err)
		}
		mods := []entroq.ModifyArg{
			task.Delete(),
			entroq.InsertingInto(ack.ReplyQueue, entroq.WithRawValue(value)),
		}
		if resp.Final {
			mods = append(mods, docs[0].Delete())
		} else {
			mods = append(mods, docs[0].Change(entroq.WithDocArrivalTimeBy(entroq.DefaultClaimDuration)))
		}

		result := worker.Modify(mods...)
		if consumedBody {
			result = result.OnDependency(func(_ context.Context, err *entroq.DependencyError) error {
				return worker.FatalErrorf("session %q response commit lost dependency after socket read: %v", ack.Session, err)
			})
		}
		return result.OnSuccess(func(context.Context) error {
			if resp.Final {
				r.recordSession(ctx, start, state, ack.ReplyQueue)
				state.responseCompleted = true
				complete()
				return nil
			}
			if localSwitched {
				state.responseWorkerSwitched = true
				state.responseWorkerCancel()
			}
			return nil
		}), nil
	}
}

func (r *Receiver) recordSession(ctx context.Context, start sessionStart, state *receiverSessionState, responseQueue string) {
	duration := time.Since(start.startedAt).Seconds()
	r.handled.Add(ctx, 1)
	r.duration.Record(ctx, duration)
	if r.cfg.auditLog != nil {
		r.cfg.auditLog.LogAttrs(ctx, slog.LevelInfo, "request_handled",
			slog.String("queue", r.cfg.name),
			slog.String("response_queue", responseQueue),
			slog.String("method", start.method),
			slog.String("path", start.path),
			slog.Int("status_code", state.statusCode),
			slog.Float64("duration_s", duration),
		)
	}
}

func hasRequestMetadata(env Envelope) bool {
	return env.ResponseQueue != "" || env.Method != "" || env.Path != "" || env.ProtocolMajor != 0 || env.ContentLength != 0 || len(env.Headers) != 0 || len(env.TrailerKeys) != 0
}

func connectionDocNamespace(prefix string) string {
	return path.Join(prefix, "connections", "gc=")
}
