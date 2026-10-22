package async

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	sender        *Sender
	writer        http.ResponseWriter
	session       string
	bootstrapTask *entroq.Task
	source        *bodySource

	requestLanes  sessionLanes
	responseLanes sessionLanes
	startedAt     time.Time
	liveness      *peerLiveness

	responseStarted bool
	responseStatus  int
	responseErr     error
}

// claimant is the consumer a session's workers hold things as: the connection
// itself, for the reasons on Receiver.claimant. A session hands one exchange
// between its workers and also touches those tasks through the sender's client
// directly, so splitting the two would split the consumer.
func (s *senderSession) claimant() string {
	return s.sender.eq.ID()
}

func newSenderSession(sender *Sender, writer http.ResponseWriter, request *http.Request, session string, requestAck, responseData *receiveLane, bootstrapTask *entroq.Task, startedAt time.Time) *senderSession {
	return &senderSession{
		sender:        sender,
		writer:        writer,
		session:       session,
		bootstrapTask: bootstrapTask,
		source:        newBodySource(request.Body, request.Trailer, releaseBody(writer, request.Body)),
		requestLanes:  sessionLanes{local: requestAck},
		responseLanes: sessionLanes{local: responseData},
		startedAt:     startedAt,
		liveness:      newPeerLiveness(sender.heartbeat),
	}
}

// releaseBody returns a func that ends a Read of the request body that is
// waiting on the client. Closing the body cannot do it alone: an HTTP/1 server
// body's Close waits for a Read in progress. A read deadline on the connection
// ends the Read; a writer without deadlines, such as a test recorder, gets the
// Close.
func releaseBody(w http.ResponseWriter, body io.Closer) func() {
	rc := http.NewResponseController(w)
	return func() {
		err := rc.SetReadDeadline(time.Now())
		if err == nil {
			return
		}
		if !errors.Is(err, http.ErrNotSupported) {
			log.Printf("release request body: %v", err)
		}
		if err := body.Close(); err != nil {
			log.Printf("close request body: %v", err)
		}
	}
}

// sessionEnd is a session ending on its own terms: the terminal response
// committed, or writing it to the caller failed and the peer was told. err is
// the remote error that arrived after the response had started, if one did.
type sessionEnd struct {
	err error
}

func (e *sessionEnd) Error() string { return "eqlink session ended" }

// run carries the exchange until a worker ends it, each returning what ended
// it: the response lane a *sessionEnd, liveness errPeerSilent, a lane its
// failure. With none of those, the caller's context ended it. The peer is told
// unless the response finished or the peer itself has gone quiet.
func (s *senderSession) run(parent context.Context) error {
	g, gctx := errgroup.WithContext(parent)
	g.Go(func() error { return s.source.run(gctx) })
	g.Go(func() error { return s.runRequestLane(gctx) })
	g.Go(func() error { return s.runResponseLane(gctx) })
	g.Go(func() error { return s.liveness.run(gctx) })

	err := g.Wait()
	s.deleteBootstrapTask(parent)
	if err == nil {
		// No worker decided the end, so the caller's context did.
		err = context.Cause(parent)
	}
	var end *sessionEnd
	switch {
	case errors.As(err, &end):
		return end.err
	case errors.Is(err, errPeerSilent):
		return errSenderPeerTimeout
	case err == nil:
		return fmt.Errorf("session workers exited before terminal response committed")
	}
	s.notifyPeerCanceled(parent, err)
	return err
}

// deleteBootstrapTask removes unaccepted demand after a local cancellation or
// failure. A dependency error means the receiver already claimed or consumed
// the task and owns the session's remaining cleanup.
func (s *senderSession) deleteBootstrapTask(parent context.Context) {
	if s.bootstrapTask == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	if _, err := s.sender.eq.Modify(ctx, s.bootstrapTask.Delete()); err != nil {
		if _, ok := entroq.AsDependency(err); ok {
			return
		}
		log.Printf("sender session %q delete unaccepted inbox task: %v", s.session, err)
	}
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

// runRequestLane claims request ACKs, answering each with the next frame of
// the request body.
func (s *senderSession) runRequestLane(ctx context.Context) error {
	_, err := runLane(ctx, s.sender.eq, s.sender.mp, s.claimant(), &s.requestLanes, noDocs[Envelope],
		func(turns *laneTurns) worker.DoModifyRun[Envelope] {
			return func(ctx context.Context, _ entroq.Reader, work *worker.Work[Envelope]) (*worker.Result, error) {
				return s.handleRequestAck(ctx, turns, work)
			}
		})
	if errors.Is(err, errLaneExpired) {
		return errSenderQueueExpired
	}
	if err != nil {
		return fmt.Errorf("request ACK worker: %w", err)
	}
	return nil
}

func (s *senderSession) handleRequestAck(ctx context.Context, turns *laneTurns, work *worker.Work[Envelope]) (*worker.Result, error) {
	task, ack := work.Task, work.Value
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
		return turns.end(worker.Modify(task.Delete()), turnDone, func(context.Context) error {
			s.source.release()
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
	next := turnNext
	switch {
	case env.Final:
		next = turnDone
	case localSwitched:
		next = turnRotated
	}
	return turns.end(result, next, nil), nil
}

// runResponseLane claims response frames, writing each to the caller and
// answering it with an ACK. It returns a *sessionEnd once the response is
// done, which ends the session.
func (s *senderSession) runResponseLane(ctx context.Context) error {
	done, err := runLane(ctx, s.sender.eq, s.sender.mp, s.claimant(), &s.responseLanes, noDocs[Response],
		func(turns *laneTurns) worker.DoModifyRun[Response] {
			return func(ctx context.Context, _ entroq.Reader, work *worker.Work[Response]) (*worker.Result, error) {
				return s.handleResponse(ctx, turns, work)
			}
		})
	if errors.Is(err, errLaneExpired) {
		return errSenderQueueExpired
	}
	if err != nil {
		return fmt.Errorf("response data worker: %w", err)
	}
	if done {
		return &sessionEnd{err: s.responseErr}
	}
	return nil
}

func (s *senderSession) handleResponse(ctx context.Context, turns *laneTurns, work *worker.Work[Response]) (*worker.Result, error) {
	task, resp := work.Task, work.Value
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
			result := worker.Modify(mods...).OnDependency(func(_ context.Context, depErr *entroq.DependencyError) error {
				return worker.FatalErrorf("session %q response failure commit lost dependency: %v", s.session, depErr)
			})
			return turns.end(result, turnDone, nil), nil
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
	if !resp.Final {
		next := turnNext
		if localSwitched {
			next = turnRotated
		}
		return turns.end(result, next, nil), nil
	}
	return turns.end(result, turnDone, func(context.Context) error {
		if s.sender.auditLog != nil {
			s.sender.auditLog.LogAttrs(ctx, slog.LevelInfo, "response_received",
				slog.String("response_queue", s.responseLanes.local.queue),
				slog.Int("status_code", s.responseStatus),
				slog.Float64("duration_s", time.Since(s.startedAt).Seconds()),
			)
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
