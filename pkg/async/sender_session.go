package async

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
	"golang.org/x/sync/errgroup"
)

var (
	errSenderPeerTimeout  = errors.New("eqlink peer liveness timeout")
	errSenderQueueExpired = errors.New("eqlink session queue expired")
)

type senderSession struct {
	sender  *Sender
	writer  http.ResponseWriter
	session string
	source  *bodySource

	requestLanes  sessionLanes
	responseLanes sessionLanes
	startedAt     time.Time
	liveness      *peerLiveness
	cancel        context.CancelCauseFunc

	requestWorkerCancel    context.CancelFunc
	responseWorkerCancel   context.CancelFunc
	requestWorkerSwitched  bool
	responseWorkerSwitched bool
	requestCompleted       bool
	responseCompleted      bool

	responseStarted bool
	responseStatus  int
	responseErr     error
}

func newSenderSession(sender *Sender, writer http.ResponseWriter, request *http.Request, session string, requestAck, responseData *receiveLane, startedAt time.Time) *senderSession {
	return &senderSession{
		sender:        sender,
		writer:        writer,
		session:       session,
		source:        newBodySource(request.Body, request.Trailer),
		requestLanes:  sessionLanes{local: requestAck},
		responseLanes: sessionLanes{local: responseData},
		startedAt:     startedAt,
		liveness:      newPeerLiveness(sender.heartbeat),
	}
}

func (s *senderSession) run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	s.cancel = cancel

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.source.run(gctx) })
	g.Go(func() error { return s.runRequestWorkers(gctx) })
	g.Go(func() error { return s.runResponseWorkers(gctx) })
	g.Go(func() error {
		return s.liveness.run(gctx, func() { cancel(errSenderPeerTimeout) })
	})

	err := g.Wait()
	if s.responseCompleted {
		return s.responseErr
	}
	if err != nil {
		if parent.Err() != nil {
			s.notifyPeerCanceled(parent, err)
		}
		return err
	}
	if cause := context.Cause(ctx); cause != nil {
		if parent.Err() != nil {
			s.notifyPeerCanceled(parent, cause)
		}
		return cause
	}
	return fmt.Errorf("session workers exited before terminal response committed")
}

// notifyPeerCanceled is a best-effort courtesy. The request data queue is the
// only control route guaranteed not to be occupied by an already-claimed ACK;
// if the request lane has ended, queue GC remains the terminal fallback.
func (s *senderSession) notifyPeerCanceled(parent context.Context, cause error) {
	if s.requestLanes.peer.queue == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	if _, err := s.sender.eq.Modify(ctx, entroq.InsertingInto(s.requestLanes.peer.queue, entroq.WithValue(Envelope{
		FrameControl: FrameControl{
			Session: s.session,
			Final:   true,
			Error:   cause.Error(),
		},
	}))); err != nil {
		log.Printf("sender session %q notify peer cancellation: %v", s.session, err)
	}
}

func (s *senderSession) runRequestWorkers(ctx context.Context) error {
	for {
		workerCtx, stopWorker := context.WithDeadline(ctx, s.requestLanes.local.collectAt)
		s.requestWorkerCancel = stopWorker
		s.requestWorkerSwitched = false
		requestWorker := worker.New(s.sender.eq,
			worker.WithDoModify(s.handleRequestAck),
			worker.WithMeterProvider[Envelope](s.sender.mp),
		)
		err := requestWorker.Run(workerCtx, worker.Watching(s.requestLanes.local.queue))
		workerErr := workerCtx.Err()
		stopWorker()
		if err != nil {
			return fmt.Errorf("request ACK worker: %w", err)
		}
		if s.requestCompleted || ctx.Err() != nil {
			return nil
		}
		if errors.Is(workerErr, context.DeadlineExceeded) {
			return errSenderQueueExpired
		}
		if !s.requestWorkerSwitched {
			return fmt.Errorf("request ACK worker exited without completing or switching queues")
		}
	}
}

func (s *senderSession) handleRequestAck(ctx context.Context, task *entroq.Task, ack Envelope, _ []*entroq.Doc) (*worker.Result, error) {
	if ack.Session != s.session {
		return nil, worker.FatalErrorf("request ACK session mismatch: got %q, want %q", ack.Session, s.session)
	}
	if !envelopeDataEmpty(ack) {
		return nil, worker.FatalErrorf("session %q received request data where an ACK was expected", s.session)
	}
	s.liveness.observed()
	if ack.Final {
		if ack.Error == "" || ack.ReplyQueue != "" {
			return nil, worker.FatalErrorf("session %q received invalid terminal request ACK", s.session)
		}
		return worker.Modify(task.Delete()).OnSuccess(func(context.Context) error {
			s.requestCompleted = true
			s.source.body.Close()
			s.requestWorkerCancel()
			return nil
		}), nil
	}
	if ack.Error != "" || ack.ReplyQueue == "" {
		return nil, worker.FatalErrorf("session %q received invalid request ACK", s.session)
	}

	reciprocate, err := s.requestLanes.observePeer(ack.ReplyQueue)
	if err != nil {
		return nil, worker.FatalErrorf("session %q request lane: %v", s.session, err)
	}

	var (
		env           Envelope
		localSwitched bool
		consumedBody  bool
	)
	if reciprocate {
		s.requestLanes.reciprocateSwitch(time.Now())
		localSwitched = true
		env.FrameControl = FrameControl{Session: s.session, ReplyQueue: s.requestLanes.local.queue}
	} else {
		forceAt := s.requestLanes.peer.forceAt(s.sender.laneTiming)
		heartbeatAt := time.Now().Add(s.sender.heartbeat.after)
		event, idle, err := s.source.nextBefore(ctx, earlier(forceAt, heartbeatAt))
		if err != nil {
			return nil, err
		}
		if idle {
			if !heartbeatAt.Before(forceAt) {
				s.requestLanes.initiateSwitch(time.Now())
				localSwitched = true
			}
			env.FrameControl = FrameControl{Session: s.session, ReplyQueue: s.requestLanes.local.queue}
		} else {
			consumedBody = true
			env = Envelope{
				FrameControl: FrameControl{
					Session: s.session,
					Final:   event.end,
				},
				Body:     event.body,
				Trailers: event.trailers,
			}
			if event.err != nil {
				env.Error = event.err.Error()
			}
			if !env.Final {
				switch {
				case s.requestLanes.local.shouldPiggyback(time.Now()):
					s.requestLanes.initiateSwitch(time.Now())
					localSwitched = true
				}
				env.ReplyQueue = s.requestLanes.local.queue
			}
		}
	}

	value, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshal request frame: %w", err)
	}
	result := worker.Modify(
		task.Delete(),
		entroq.InsertingInto(ack.ReplyQueue, entroq.WithRawValue(value)),
	)
	if consumedBody {
		result = result.OnDependency(func(_ context.Context, err *entroq.DependencyError) error {
			return worker.FatalErrorf("session %q request commit lost dependency after body read: %v", s.session, err)
		})
	}
	return result.OnSuccess(func(context.Context) error {
		if env.Final {
			s.requestCompleted = true
			s.requestWorkerCancel()
		} else if localSwitched {
			s.requestWorkerSwitched = true
			s.requestWorkerCancel()
		}
		return nil
	}), nil
}

func (s *senderSession) runResponseWorkers(ctx context.Context) error {
	for {
		workerCtx, stopWorker := context.WithDeadline(ctx, s.responseLanes.local.collectAt)
		s.responseWorkerCancel = stopWorker
		s.responseWorkerSwitched = false
		responseWorker := worker.New(s.sender.eq,
			worker.WithDoModify(s.handleResponse),
			worker.WithMeterProvider[Response](s.sender.mp),
		)
		err := responseWorker.Run(workerCtx, worker.Watching(s.responseLanes.local.queue))
		workerErr := workerCtx.Err()
		stopWorker()
		if err != nil {
			return fmt.Errorf("response data worker: %w", err)
		}
		if s.responseCompleted || ctx.Err() != nil {
			return nil
		}
		if errors.Is(workerErr, context.DeadlineExceeded) {
			return errSenderQueueExpired
		}
		if !s.responseWorkerSwitched {
			return fmt.Errorf("response data worker exited without completing or switching queues")
		}
	}
}

func (s *senderSession) handleResponse(ctx context.Context, task *entroq.Task, resp Response, _ []*entroq.Doc) (*worker.Result, error) {
	if resp.Session != s.session {
		return nil, worker.FatalErrorf("response session mismatch: got %q, want %q", resp.Session, s.session)
	}
	if !resp.Final && resp.ReplyQueue == "" {
		return nil, worker.FatalErrorf("session %q received response data without an ACK queue", s.session)
	}
	if resp.Final && resp.ReplyQueue != "" {
		return nil, worker.FatalErrorf("session %q received terminal response with an ACK queue", s.session)
	}
	if resp.Error != "" && !resp.Final {
		return nil, worker.FatalErrorf("session %q received nonterminal response error", s.session)
	}
	s.liveness.observed()

	reciprocate := false
	if !resp.Final {
		var err error
		reciprocate, err = s.responseLanes.observePeer(resp.ReplyQueue)
		if err != nil {
			return nil, worker.FatalErrorf("session %q response lane: %v", s.session, err)
		}
	}

	controlOnly := emptyResponse(resp)
	responseWasStarted := s.responseStarted
	if !s.responseStarted && resp.StatusCode != 0 {
		for key, values := range resp.Headers {
			for _, value := range values {
				s.writer.Header().Add(key, value)
			}
		}
		declareTrailers(s.writer.Header(), resp.TrailerKeys)
		s.writer.WriteHeader(resp.StatusCode)
		s.responseStarted = true
		s.responseStatus = resp.StatusCode
	} else if s.responseStarted && (resp.StatusCode != 0 || len(resp.Headers) != 0 || len(resp.TrailerKeys) != 0) {
		return nil, worker.FatalErrorf("session %q received repeated response metadata", s.session)
	} else if !s.responseStarted && !controlOnly {
		return nil, worker.FatalErrorf("session %q response body arrived before response metadata", s.session)
	}

	if len(resp.Body) > 0 {
		if _, err := s.writer.Write(resp.Body); err != nil {
			mods := []entroq.ModifyArg{task.Delete()}
			if resp.ReplyQueue != "" {
				mods = append(mods, entroq.InsertingInto(resp.ReplyQueue, entroq.WithValue(Response{FrameControl: FrameControl{
					Session: s.session,
					Final:   true,
					Error:   err.Error(),
				}})))
			}
			return worker.Modify(mods...).OnDependency(func(_ context.Context, depErr *entroq.DependencyError) error {
				return worker.FatalErrorf("session %q response failure commit lost dependency: %v", s.session, depErr)
			}).OnSuccess(func(context.Context) error {
				s.responseCompleted = true
				s.cancel(err)
				return nil
			}), nil
		}
	}
	if resp.Final {
		applyTrailers(s.writer.Header(), resp.Trailers)
	}
	if resp.Error != "" {
		if responseWasStarted {
			s.responseErr = errors.New(resp.Error)
		}
		log.Printf("sender remote error for session %q: %s", s.session, resp.Error)
	}

	mods := []entroq.ModifyArg{task.Delete()}
	localSwitched := false
	if !resp.Final {
		if s.responseStarted && !controlOnly {
			if err := http.NewResponseController(s.writer).Flush(); err != nil {
				return nil, worker.FatalErrorf("session %q flush response: %v", s.session, err)
			}
		}
		switch {
		case reciprocate:
			s.responseLanes.reciprocateSwitch(time.Now())
			localSwitched = true
		case s.responseLanes.local.shouldPiggyback(time.Now()):
			s.responseLanes.initiateSwitch(time.Now())
			localSwitched = true
		}
		mods = append(mods, entroq.InsertingInto(resp.ReplyQueue, entroq.WithValue(Response{FrameControl: FrameControl{
			Session:    s.session,
			ReplyQueue: s.responseLanes.local.queue,
		}})))
	}

	result := worker.Modify(mods...).OnDependency(func(_ context.Context, err *entroq.DependencyError) error {
		return worker.FatalErrorf("session %q response commit lost dependency after socket write: %v", s.session, err)
	})
	return result.OnSuccess(func(context.Context) error {
		if resp.Final {
			s.responseCompleted = true
			if s.sender.auditLog != nil {
				s.sender.auditLog.LogAttrs(ctx, slog.LevelInfo, "response_received",
					slog.String("response_queue", s.responseLanes.local.queue),
					slog.Int("status_code", s.responseStatus),
					slog.Float64("duration_s", time.Since(s.startedAt).Seconds()),
				)
			}
			s.cancel(s.responseErr)
			return nil
		}
		if localSwitched {
			s.responseWorkerSwitched = true
			s.responseWorkerCancel()
		}
		return nil
	}), nil
}

func declareTrailers(header http.Header, keys []string) {
	if len(keys) > 0 {
		header.Set("Trailer", strings.Join(keys, ", "))
	}
}

func applyTrailers(header http.Header, trailers http.Header) {
	for key, values := range trailers {
		header[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
}

func emptyResponse(resp Response) bool {
	return resp.StatusCode == 0 && len(resp.Headers) == 0 && len(resp.TrailerKeys) == 0 && len(resp.Body) == 0 && len(resp.Trailers) == 0 && resp.Error == ""
}

func responseDataEmpty(resp Response) bool {
	return resp.StatusCode == 0 && len(resp.Headers) == 0 && len(resp.TrailerKeys) == 0 && len(resp.Body) == 0 && len(resp.Trailers) == 0
}

func envelopeDataEmpty(env Envelope) bool {
	return env.ResponseQueue == "" && env.Method == "" && env.Path == "" && env.ProtocolMajor == 0 && env.ContentLength == 0 && len(env.Headers) == 0 && len(env.TrailerKeys) == 0 && len(env.Body) == 0 && len(env.Trailers) == 0
}
