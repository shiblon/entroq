package async

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
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

// ReceiverOption configures a Receiver.
type ReceiverOption func(*receiverConfig)

type receiverConfig struct {
	mp          metric.MeterProvider
	httpClient  *http.Client
	name        string
	auditLog    *slog.Logger
	concurrency int
	laneTiming  laneTiming
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
	lane    *receiveLane
	docNS   string
	session string
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

// WithReceiverAuditLogger enables structured audit logging. When set, one
// request_handled event is emitted after an HTTP session completes.
func WithReceiverAuditLogger(l *slog.Logger) ReceiverOption {
	return func(c *receiverConfig) {
		c.auditLog = l
	}
}

// WithReceiverConcurrency sets the number of workers accepting new sessions
// from the primary service inbox. Active sessions run in independent workers
// after bootstrap and do not consume these slots. Defaults to one.
func WithReceiverConcurrency(n int) ReceiverOption {
	return func(c *receiverConfig) {
		c.concurrency = n
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
		concurrency: 1,
		laneTiming:  defaultLaneTiming,
	}
	for _, o := range opts {
		o(cfg)
	}

	m := cfg.mp.Meter("entroq/async/receiver")
	handled, _ := m.Int64Counter("receiver.handled_total",
		metric.WithDescription("Total number of HTTP sessions handled by the receiver."),
	)
	forwardErrors, _ := m.Int64Counter("receiver.forward_errors_total",
		metric.WithDescription("Total number of upstream forwarding errors encountered by the receiver."),
	)
	duration, _ := m.Float64Histogram("receiver.duration_seconds",
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
		var peer peerLane
		if _, err := peer.observe(env.ReplyQueue); err != nil {
			return nil, worker.MoveErrorf("eqlink initial frame: %v", err)
		}

		prefix := path.Dir(task.Queue)
		lane := newReceiveLane(prefix, env.Session, "request", time.Now(), r.cfg.laneTiming)
		start := sessionStart{
			lane:    lane,
			docNS:   connectionDocNamespace(prefix),
			session: env.Session,
		}
		value, err := json.Marshal(env)
		if err != nil {
			return nil, fmt.Errorf("receiver bootstrap marshal frame: %w", err)
		}

		return worker.Modify(
			task.Delete(),
			entroq.InsertingInto(start.lane.queue, entroq.WithRawValue(value)),
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
	ctx, cancel := context.WithCancel(ctx)
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

	socket := newResponseSocket()
	state := &receiverSessionState{lanes: sessionLanes{local: start.lane}}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		if err := socket.run(gctx, r.cfg.httpClient, r.upstream); err != nil {
			return fmt.Errorf("upstream socket: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		return r.runSessionWorkers(gctx, start, state, socket, complete)
	})
	if err := g.Wait(); err != nil {
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

type receiverSessionState struct {
	lanes          sessionLanes
	opened         bool
	completed      bool
	workerSwitched bool
	workerCancel   context.CancelFunc
	startedAt      time.Time
	method         string
	path           string
	statusCode     int
}

func (r *Receiver) runSessionWorkers(ctx context.Context, start sessionStart, state *receiverSessionState, socket *responseSocket, complete func()) error {
	for {
		workerCtx, stopWorker := context.WithDeadline(ctx, state.lanes.local.collectAt)
		state.workerCancel = stopWorker
		state.workerSwitched = false
		sessionWorker := worker.New(r.eq,
			worker.WithTakeDocs(func(_ context.Context, _ *entroq.Task, env Envelope) ([]*entroq.DocClaim, error) {
				if env.Session != start.session {
					return nil, worker.FatalErrorf("session mismatch: frame %q, worker %q", env.Session, start.session)
				}
				return []*entroq.DocClaim{entroq.ClaimKey(start.docNS, start.session)}, nil
			}),
			worker.WithDoModify(r.sessionHandler(state, socket, complete)),
			worker.WithMeterProvider[Envelope](r.cfg.mp),
		)
		err := sessionWorker.Run(workerCtx, worker.Watching(state.lanes.local.queue))
		workerErr := workerCtx.Err()
		stopWorker()
		if err != nil {
			return fmt.Errorf("session worker: %w", err)
		}
		if state.completed || ctx.Err() != nil {
			return nil
		}
		if errors.Is(workerErr, context.DeadlineExceeded) {
			log.Printf("receiver session %q queue %q expired while awaiting its peer; abandoning the local socket", start.session, state.lanes.local.queue)
			state.completed = true
			complete()
			return nil
		}
		if !state.workerSwitched {
			return fmt.Errorf("session worker exited without completing or switching queues")
		}
	}
}

func (r *Receiver) sessionHandler(state *receiverSessionState, socket *responseSocket, complete func()) worker.DoModifyRun[Envelope] {
	return func(ctx context.Context, task *entroq.Task, env Envelope, docs []*entroq.Doc) (*worker.Result, error) {
		if len(docs) != 1 {
			return nil, worker.FatalErrorf("session %q claimed %d connection docs", env.Session, len(docs))
		}
		if env.Final {
			if env.Error == "" {
				return nil, worker.FatalErrorf("session %q received a terminal request without an error", env.Session)
			}
			if env.ReplyQueue != "" {
				return nil, worker.FatalErrorf("session %q received a terminal request with a reply queue", env.Session)
			}
			log.Printf("receiver session %q canceled by peer: %s", env.Session, env.Error)
			return worker.Modify(task.Delete(), docs[0].Delete()).OnDependency(func(_ context.Context, err *entroq.DependencyError) error {
				return worker.FatalErrorf("session %q terminal commit lost dependency: %v", env.Session, err)
			}).OnSuccess(func(context.Context) error {
				state.completed = true
				complete()
				return nil
			}), nil
		}
		if env.ReplyQueue == "" {
			return nil, worker.FatalErrorf("session %q received a non-final frame without a reply queue", env.Session)
		}
		if env.Error != "" {
			return nil, worker.FatalErrorf("session %q received a non-final request error", env.Session)
		}
		if state.opened && !emptyEnvelope(env) {
			return nil, worker.FatalErrorf("session %q request streaming is not supported", env.Session)
		}
		reciprocate, err := state.lanes.observePeer(env.ReplyQueue)
		if err != nil {
			return nil, worker.FatalErrorf("session %q: %v", env.Session, err)
		}

		if !state.opened {
			state.startedAt = time.Now()
			state.method = env.Method
			state.path = env.Path
			if err := socket.open(ctx, env); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				log.Printf("receiver open %s%s: %v", r.upstream, env.Path, err)
				r.forwardErrors.Add(ctx, 1)
				resp := responseForSocketError(env.Session, err)
				value, marshalErr := json.Marshal(resp)
				if marshalErr != nil {
					return nil, fmt.Errorf("receiver marshal open error: %w", marshalErr)
				}
				return worker.Modify(
					task.Delete(),
					docs[0].Delete(),
					entroq.InsertingInto(env.ReplyQueue, entroq.WithRawValue(value)),
				).OnDependency(func(_ context.Context, err *entroq.DependencyError) error {
					return worker.FatalErrorf("session %q open-error commit lost dependency: %v", env.Session, err)
				}).OnSuccess(func(context.Context) error {
					state.statusCode = resp.StatusCode
					r.recordSession(ctx, state, env.ReplyQueue)
					complete()
					return nil
				}), nil
			}
			state.opened = true
		}

		var (
			resp          Response
			localSwitched bool
		)
		if reciprocate && emptyEnvelope(env) {
			// A data-free switch is answered immediately with a data-free switch,
			// returning the turn to the peer that was waiting on local I/O.
			state.lanes.reciprocateSwitch(time.Now())
			localSwitched = true
			resp.FrameControl = FrameControl{Session: env.Session, ReplyQueue: state.lanes.local.queue}
		} else {
			event, forced, err := socket.nextBefore(ctx, state.lanes.peer.forceAt(r.cfg.laneTiming))
			if err != nil {
				return nil, err
			}
			if forced {
				state.lanes.initiateSwitch(time.Now())
				localSwitched = true
				resp.FrameControl = FrameControl{Session: env.Session, ReplyQueue: state.lanes.local.queue}
			} else {
				resp = Response{
					FrameControl: FrameControl{
						Session: env.Session,
						Final:   event.final || event.err != nil,
					},
					StatusCode: event.statusCode,
					Headers:    event.headers,
					Body:       event.body,
				}
				if event.statusCode != 0 {
					state.statusCode = event.statusCode
				}
				if !resp.Final {
					switch {
					case reciprocate:
						state.lanes.reciprocateSwitch(time.Now())
						localSwitched = true
					case state.lanes.local.shouldPiggyback(time.Now()):
						state.lanes.initiateSwitch(time.Now())
						localSwitched = true
					}
					resp.ReplyQueue = state.lanes.local.queue
				}
				if event.err != nil {
					resp.Error = event.err.Error()
					log.Printf("receiver read %s%s: %v", r.upstream, env.Path, event.err)
					r.forwardErrors.Add(ctx, 1)
				}
			}
		}
		respValue, err := json.Marshal(resp)
		if err != nil {
			return nil, fmt.Errorf("receiver marshal response: %w", err)
		}

		mods := []entroq.ModifyArg{
			task.Delete(),
			entroq.InsertingInto(env.ReplyQueue, entroq.WithRawValue(respValue)),
		}
		if resp.Final {
			mods = append(mods, docs[0].Delete())
		} else {
			mods = append(mods, docs[0].Change(
				entroq.WithDocArrivalTimeBy(entroq.DefaultClaimDuration),
			))
		}

		// The socket event has been consumed and cannot be replayed if this
		// atomic commit loses a dependency. Stop instead of reclaiming the task
		// and silently advancing to the next event.
		result := worker.Modify(mods...).OnDependency(func(_ context.Context, err *entroq.DependencyError) error {
			return worker.FatalErrorf("session %q frame commit lost dependency after socket read: %v", env.Session, err)
		})
		return result.OnSuccess(func(context.Context) error {
			if resp.Final {
				r.recordSession(ctx, state, env.ReplyQueue)
				state.completed = true
				complete()
				return nil
			}
			if localSwitched {
				state.workerSwitched = true
				state.workerCancel()
			}
			return nil
		}), nil
	}
}

func (r *Receiver) recordSession(ctx context.Context, state *receiverSessionState, responseQueue string) {
	duration := time.Since(state.startedAt).Seconds()
	r.handled.Add(ctx, 1)
	r.duration.Record(ctx, duration)
	if r.cfg.auditLog != nil {
		r.cfg.auditLog.LogAttrs(ctx, slog.LevelInfo, "request_handled",
			slog.String("queue", r.cfg.name),
			slog.String("response_queue", responseQueue),
			slog.String("method", state.method),
			slog.String("path", state.path),
			slog.Int("status_code", state.statusCode),
			slog.Float64("duration_s", duration),
		)
	}
}

func emptyEnvelope(env Envelope) bool {
	return env.Method == "" && env.Path == "" && len(env.Headers) == 0 && len(env.Body) == 0 && env.Error == ""
}

func connectionDocNamespace(prefix string) string {
	return path.Join(prefix, "connections", "gc=")
}
