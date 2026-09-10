package async

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/worker"
	"golang.org/x/sync/errgroup"
)

var (
	errSenderIdleTimeout  = errors.New("eqlink response idle timeout")
	errSenderQueueExpired = errors.New("eqlink response queue expired")
)

type senderSession struct {
	sender       *Sender
	writer       http.ResponseWriter
	session      string
	lanes        sessionLanes
	startedAt    time.Time
	activity     chan struct{}
	cancel       context.CancelCauseFunc
	workerCancel context.CancelFunc

	responseStarted bool
	responseStatus  int
	completed       bool
	workerSwitched  bool
}

func newSenderSession(sender *Sender, writer http.ResponseWriter, session string, local *receiveLane, startedAt time.Time) *senderSession {
	return &senderSession{
		sender:    sender,
		writer:    writer,
		session:   session,
		lanes:     sessionLanes{local: local},
		startedAt: startedAt,
		activity:  make(chan struct{}, 1),
	}
}

func (s *senderSession) run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	s.cancel = cancel

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return s.runWorkers(gctx)
	})
	g.Go(func() error {
		timer := time.NewTimer(s.sender.requestTimeout)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				// Prefer already-committed activity over a simultaneous timer
				// wakeup. If activity arrives after this check, the idle
				// deadline genuinely won the race.
				select {
				case <-s.activity:
					timer.Reset(s.sender.requestTimeout)
					continue
				default:
				}
				cancel(errSenderIdleTimeout)
				return nil
			case <-s.activity:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(s.sender.requestTimeout)
			case <-gctx.Done():
				return nil
			}
		}
	})

	err := g.Wait()
	if s.completed {
		return nil
	}
	if err != nil {
		if parent.Err() != nil {
			s.notifyPeerCanceled(parent, err)
		}
		return err
	}
	if cause := context.Cause(ctx); cause != nil {
		// An EQLink idle timeout must return its 504 promptly. A canceled local
		// HTTP context no longer has a waiting caller, so it can afford this
		// best-effort courtesy without changing observable timeout behavior.
		if parent.Err() != nil {
			s.notifyPeerCanceled(parent, cause)
		}
		return cause
	}
	return fmt.Errorf("response worker exited before terminal frame committed")
}

func (s *senderSession) notifyPeerCanceled(parent context.Context, cause error) {
	if s.lanes.peer.queue == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	if _, err := s.sender.eq.Modify(ctx, entroq.InsertingInto(s.lanes.peer.queue, entroq.WithValue(Envelope{
		FrameControl: FrameControl{
			Session: s.session,
			Final:   true,
			Error:   cause.Error(),
		},
	}))); err != nil {
		log.Printf("sender session %q notify peer cancellation: %v", s.session, err)
	}
}

// runWorkers replaces the worker only after a frame advertising a new local
// inbox commits. Each individual worker still owns the complete claim, renew,
// and modify lifecycle for its queue generation.
func (s *senderSession) runWorkers(ctx context.Context) error {
	for {
		workerCtx, stopWorker := context.WithDeadline(ctx, s.lanes.local.collectAt)
		s.workerCancel = stopWorker
		s.workerSwitched = false
		responseWorker := worker.New(s.sender.eq,
			worker.WithDoModify(s.handle),
			worker.WithMeterProvider[Response](s.sender.mp),
		)
		err := responseWorker.Run(workerCtx, worker.Watching(s.lanes.local.queue))
		workerErr := workerCtx.Err()
		stopWorker()
		if err != nil {
			return fmt.Errorf("response worker: %w", err)
		}
		if s.completed || ctx.Err() != nil {
			return nil
		}
		if errors.Is(workerErr, context.DeadlineExceeded) {
			return errSenderQueueExpired
		}
		if !s.workerSwitched {
			return fmt.Errorf("response worker exited without completing or switching queues")
		}
	}
}

func (s *senderSession) handle(ctx context.Context, task *entroq.Task, resp Response, _ []*entroq.Doc) (*worker.Result, error) {
	if resp.Session != s.session {
		return nil, worker.FatalErrorf("response session mismatch: got %q, want %q", resp.Session, s.session)
	}
	if !resp.Final && resp.ReplyQueue == "" {
		return nil, worker.FatalErrorf("session %q received a non-final response without a reply queue", s.session)
	}
	if resp.Final && resp.ReplyQueue != "" {
		return nil, worker.FatalErrorf("session %q received a final response with a reply queue", s.session)
	}
	if resp.Error != "" && !resp.Final {
		return nil, worker.FatalErrorf("session %q received a non-final error response", s.session)
	}
	reciprocate := false
	if !resp.Final {
		var err error
		reciprocate, err = s.lanes.observePeer(resp.ReplyQueue)
		if err != nil {
			return nil, worker.FatalErrorf("session %q: %v", s.session, err)
		}
	}

	if !s.responseStarted {
		if resp.StatusCode == 0 {
			return nil, worker.FatalErrorf("session %q initial response has no status", s.session)
		}
		for key, values := range resp.Headers {
			for _, value := range values {
				s.writer.Header().Add(key, value)
			}
		}
		s.writer.WriteHeader(resp.StatusCode)
		s.responseStarted = true
		s.responseStatus = resp.StatusCode
	} else if resp.StatusCode != 0 || len(resp.Headers) != 0 {
		return nil, worker.FatalErrorf("session %q received repeated response metadata", s.session)
	}

	if len(resp.Body) > 0 {
		if _, err := s.writer.Write(resp.Body); err != nil {
			return nil, worker.FatalErrorf("session %q write response: %v", s.session, err)
		}
	}
	if resp.Error != "" {
		log.Printf("sender remote error for session %q: %s", s.session, resp.Error)
	}

	mods := []entroq.ModifyArg{task.Delete()}
	localSwitched := false
	if !resp.Final {
		if err := http.NewResponseController(s.writer).Flush(); err != nil {
			return nil, worker.FatalErrorf("session %q flush response: %v", s.session, err)
		}
		if reciprocate {
			s.lanes.reciprocateSwitch(time.Now())
			localSwitched = true
		}
		mods = append(mods, entroq.InsertingInto(resp.ReplyQueue, entroq.WithValue(Envelope{
			FrameControl: FrameControl{
				Session:    s.session,
				ReplyQueue: s.lanes.local.queue,
			},
		})))
	}

	result := worker.Modify(mods...).OnDependency(func(_ context.Context, err *entroq.DependencyError) error {
		return worker.FatalErrorf("session %q response commit lost dependency after socket write: %v", s.session, err)
	})
	return result.OnSuccess(func(context.Context) error {
		if resp.Final {
			s.completed = true
			if s.sender.auditLog != nil {
				s.sender.auditLog.LogAttrs(ctx, slog.LevelInfo, "response_received",
					slog.String("response_queue", s.lanes.local.queue),
					slog.Int("status_code", s.responseStatus),
					slog.Float64("duration_s", time.Since(s.startedAt).Seconds()),
				)
			}
			s.cancel(nil)
			return nil
		}
		if localSwitched {
			s.workerSwitched = true
			s.workerCancel()
		}
		select {
		case s.activity <- struct{}{}:
		default:
		}
		return nil
	}), nil
}
