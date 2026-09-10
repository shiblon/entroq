package async

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/worker"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type notifyingResponseWriter struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
}

func (w *notifyingResponseWriter) Flush() {
	w.ResponseRecorder.Flush()
	select {
	case w.flushed <- struct{}{}:
	default:
	}
}

func TestSessionForcedQueueRotationReplacesWorkers(t *testing.T) {
	timing := laneTiming{
		lifetime:        3 * time.Second,
		piggybackBefore: 2 * time.Second,
		forceBefore:     time.Second,
	}
	testSessionQueueRotation(t, timing, true)
}

func TestSessionDataPiggybacksQueueRotation(t *testing.T) {
	timing := laneTiming{
		lifetime:        6 * time.Second,
		piggybackBefore: 4 * time.Second,
		forceBefore:     time.Second,
	}
	testSessionQueueRotation(t, timing, false)
}

func testSessionQueueRotation(t *testing.T, timing laneTiming, force bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("new EntroQ: %v", err)
	}
	defer eq.Close()

	responseReader, responseWriter := io.Pipe()
	defer responseReader.Close()
	defer responseWriter.Close()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       responseReader,
			Request:    req,
		}, nil
	})}

	receiverCtx, stopReceiver := context.WithCancel(ctx)
	receiver := NewReceiver(eq, "http://upstream", WithReceiverHTTPClient(client))
	receiver.cfg.laneTiming = timing
	receiverDone := make(chan error, 1)
	go func() { receiverDone <- receiver.Run(receiverCtx, "/service/inbox") }()

	const session = "rotation-session"
	local := newReceiveLane("/service", session, "response", time.Now(), timing)
	initialResponseQueue := local.queue
	sender := NewSender(eq, "", WithSenderRequestTimeout(8*time.Second))
	sender.laneTiming = timing
	w := &notifyingResponseWriter{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 8)}
	sessionState := newSenderSession(sender, w, session, local, time.Now())
	senderDone := make(chan error, 1)
	go func() { senderDone <- sessionState.run(ctx) }()

	if _, err := eq.Modify(ctx, entroq.InsertingInto("/service/inbox", entroq.WithValue(Envelope{
		FrameControl: FrameControl{Session: session, ReplyQueue: initialResponseQueue},
		Method:       http.MethodGet,
		Path:         "/events",
	}))); err != nil {
		t.Fatalf("insert initial frame: %v", err)
	}

	waitForFlush(t, ctx, w.flushed, "initial response metadata")
	if !force {
		piggybackAt := local.collectAt.Add(-timing.piggybackBefore)
		if delay := time.Until(piggybackAt.Add(1100 * time.Millisecond)); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				t.Fatal("timed out before piggyback threshold")
			}
		}
		if _, err := responseWriter.Write([]byte("data: rotated\n\n")); err != nil {
			t.Fatalf("write streaming body: %v", err)
		}
	}

	waitForFlush(t, ctx, w.flushed, "queue-switch frame")
	if force {
		if _, err := responseWriter.Write([]byte("data: after switch\n\n")); err != nil {
			t.Fatalf("write streaming body: %v", err)
		}
		waitForFlush(t, ctx, w.flushed, "post-switch body")
	}
	if err := responseWriter.Close(); err != nil {
		t.Fatalf("close streaming body: %v", err)
	}

	select {
	case err := <-senderDone:
		if err != nil {
			t.Fatalf("sender session: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("sender session did not finish")
	}
	if local.queue == initialResponseQueue {
		t.Fatal("sender continued watching its initial response queue")
	}
	if got := w.Body.String(); !strings.Contains(got, "data:") {
		t.Errorf("streaming body did not arrive: %q", got)
	}

	stopReceiver()
	select {
	case err := <-receiverDone:
		if err != nil {
			t.Fatalf("receiver run: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("receiver did not stop")
	}
}

func waitForFlush(t *testing.T, ctx context.Context, flushed <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-flushed:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", description)
	}
}

func TestSessionWorkersStopAtQueueGCDeadline(t *testing.T) {
	timing := laneTiming{
		lifetime:        2 * time.Second,
		piggybackBefore: time.Second,
		forceBefore:     500 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("new EntroQ: %v", err)
	}
	defer eq.Close()

	local := newReceiveLane("/service", "expired-session", "response", time.Now(), timing)
	sender := NewSender(eq, "", WithSenderRequestTimeout(4*time.Second))
	sender.laneTiming = timing
	session := newSenderSession(sender, httptest.NewRecorder(), "expired-session", local, time.Now())
	if err := session.run(ctx); !errors.Is(err, errSenderQueueExpired) {
		t.Fatalf("sender error: got %v, want %v", err, errSenderQueueExpired)
	}

	receiver := NewReceiver(eq, "http://upstream")
	receiver.cfg.laneTiming = timing
	receiverLane := newReceiveLane("/service", "abandoned-session", "request", time.Now(), timing)
	state := &receiverSessionState{lanes: sessionLanes{local: receiverLane}}
	completed := false
	if err := receiver.runSessionWorkers(ctx, sessionStart{
		lane:    receiverLane,
		docNS:   connectionDocNamespace("/service"),
		session: "abandoned-session",
	}, state, newResponseSocket(), func() { completed = true }); err != nil {
		t.Fatalf("receiver workers: %v", err)
	}
	if !completed {
		t.Fatal("receiver did not complete abandoned session at its queue deadline")
	}
}

func TestSenderCancellationNotifiesLastPeerQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("new EntroQ: %v", err)
	}
	defer eq.Close()

	const sessionID = "canceled-session"
	local := newReceiveLane("/service", sessionID, "response", time.Now(), defaultLaneTiming)
	peer := newReceiveLane("/service", sessionID, "request", time.Now(), defaultLaneTiming)
	sender := NewSender(eq, "")
	session := newSenderSession(sender, httptest.NewRecorder(), sessionID, local, time.Now())
	if _, err := session.lanes.observePeer(peer.queue); err != nil {
		t.Fatalf("observe peer queue: %v", err)
	}

	canceledParent, cancelParent := context.WithCancel(ctx)
	cancelParent()
	session.notifyPeerCanceled(canceledParent, context.Canceled)

	workerCtx, stopWorker := context.WithCancel(ctx)
	var got Envelope
	w := worker.New(eq, worker.WithDoModify(func(_ context.Context, task *entroq.Task, env Envelope, _ []*entroq.Doc) (*worker.Result, error) {
		got = env
		return worker.Modify(task.Delete()).OnSuccess(func(context.Context) error {
			stopWorker()
			return nil
		}), nil
	}))
	if err := w.Run(workerCtx, worker.Watching(peer.queue)); err != nil {
		t.Fatalf("receive cancellation: %v", err)
	}
	if got.Session != sessionID || !got.Final || got.Error == "" || got.ReplyQueue != "" {
		t.Errorf("cancellation frame: %+v", got.FrameControl)
	}
}

func TestReceiverSessionOwnsTaskAndConnectionDoc(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("new EntroQ: %v", err)
	}
	defer eq.Close()

	started := make(chan struct{})
	release := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("stream complete")),
			Request:    req,
		}, nil
	})}

	receiverCtx, stopReceiver := context.WithCancel(ctx)
	receiver := NewReceiver(eq, "http://upstream", WithReceiverHTTPClient(client))
	receiverDone := make(chan error, 1)
	go func() { receiverDone <- receiver.Run(receiverCtx, "/service/inbox") }()

	const session = "session-1"
	replyQueue := sessionQueue("/service", session, time.Now().Add(defaultLaneTiming.lifetime), "response")
	if _, err := eq.Modify(ctx, entroq.InsertingInto("/service/inbox", entroq.WithValue(Envelope{
		FrameControl: FrameControl{
			Session:    session,
			ReplyQueue: replyQueue,
		},
		Method: http.MethodGet,
		Path:   "/events",
	}))); err != nil {
		t.Fatalf("insert initial frame: %v", err)
	}

	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("upstream request did not start")
	}

	docNS := connectionDocNamespace("/service")
	docs, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: docNS, IDs: []string{session}})
	if err != nil {
		t.Fatalf("get connection doc: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("connection docs: got %d, want 1", len(docs))
	}
	if docs[0].Claimant != eq.ClientID {
		t.Errorf("connection doc claimant: got %q, want %q", docs[0].Claimant, eq.ClientID)
	}
	if !docs[0].At.After(time.Now()) {
		t.Errorf("connection doc is not held: At=%v", docs[0].At)
	}

	stats, err := eq.QueueStats(ctx, entroq.MatchPrefix("/service/"))
	if err != nil {
		t.Fatalf("queue stats: %v", err)
	}
	var sessionInbox string
	for name, stat := range stats {
		if strings.Contains(name, "/sess="+session+";gc=") && strings.HasSuffix(name, "/request") {
			sessionInbox = name
			if stat.Claimed != 1 {
				t.Errorf("session inbox claimed tasks: got %d, want 1", stat.Claimed)
			}
		}
	}
	if sessionInbox == "" {
		t.Fatalf("no session request inbox in stats: %v", stats)
	}

	close(release)
	response := receiveSessionResponse(ctx, t, eq, session, replyQueue)
	if response.Session != session || !response.Final {
		t.Errorf("terminal response control: got session=%q final=%v", response.Session, response.Final)
	}
	if got := string(response.Body); got != "stream complete" {
		t.Errorf("response body: got %q", got)
	}

	docs, err = eq.Docs(ctx, &entroq.DocQuery{Namespace: docNS, IDs: []string{session}})
	if err != nil {
		t.Fatalf("get final connection docs: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("connection doc remains after terminal commit: %+v", docs)
	}
	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact(sessionInbox)); err != nil {
		t.Fatalf("session inbox not empty: %v", err)
	}

	stopReceiver()
	select {
	case err := <-receiverDone:
		if err != nil {
			t.Fatalf("receiver run: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("receiver did not stop")
	}
}

func TestReceiverConcurrentSessionLifecycles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("new EntroQ: %v", err)
	}
	defer eq.Close()

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(req.URL.Path)),
			Request:    req,
		}, nil
	})}

	receiverCtx, stopReceiver := context.WithCancel(ctx)
	receiver := NewReceiver(eq, "http://upstream",
		WithReceiverHTTPClient(client),
		WithReceiverConcurrency(4),
	)
	receiverDone := make(chan error, 1)
	go func() { receiverDone <- receiver.Run(receiverCtx, "/service/inbox") }()

	const sessions = 32
	replyQueues := make([]string, 0, sessions)
	insertions := make([]entroq.ModifyArg, 0, sessions)
	for i := range sessions {
		session := fmt.Sprintf("session-%02d", i)
		replyQueue := sessionQueue("/service", session, time.Now().Add(defaultLaneTiming.lifetime), "response")
		replyQueues = append(replyQueues, replyQueue)
		insertions = append(insertions, entroq.InsertingInto("/service/inbox", entroq.WithValue(Envelope{
			FrameControl: FrameControl{Session: session, ReplyQueue: replyQueue},
			Method:       http.MethodGet,
			Path:         "/" + session,
		})))
	}
	if _, err := eq.Modify(ctx, insertions...); err != nil {
		t.Fatalf("insert initial frames: %v", err)
	}

	seen := make(map[string]bool, sessions)
	for i, replyQueue := range replyQueues {
		session := fmt.Sprintf("session-%02d", i)
		response := receiveSessionResponse(ctx, t, eq, session, replyQueue)
		if seen[response.Session] {
			t.Errorf("duplicate response for %q", response.Session)
		}
		seen[response.Session] = true
		if !response.Final {
			t.Errorf("response for %q is not final", response.Session)
		}
		if got, want := string(response.Body), "/"+response.Session; got != want {
			t.Errorf("response for %q: body got %q, want %q", response.Session, got, want)
		}
	}
	if len(seen) != sessions {
		t.Errorf("responses: got %d sessions, want %d", len(seen), sessions)
	}

	docs, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: connectionDocNamespace("/service")})
	if err != nil {
		t.Fatalf("get connection docs: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("connection docs remain after sessions completed: %d", len(docs))
	}

	stopReceiver()
	select {
	case err := <-receiverDone:
		if err != nil {
			t.Fatalf("receiver run: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("receiver did not stop")
	}
}

func receiveSessionResponse(ctx context.Context, t *testing.T, eq *entroq.EntroQ, session, replyQueue string) Response {
	t.Helper()

	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	response := Response{FrameControl: FrameControl{Session: session}}
	responseWorker := worker.New(eq, worker.WithDoModify(func(_ context.Context, task *entroq.Task, frame Response, _ []*entroq.Doc) (*worker.Result, error) {
		if frame.Session != session {
			return nil, worker.FatalErrorf("response session: got %q, want %q", frame.Session, session)
		}
		if response.StatusCode == 0 && frame.StatusCode != 0 {
			response.StatusCode = frame.StatusCode
			response.Headers = frame.Headers
		}
		response.Body = append(response.Body, frame.Body...)
		if frame.Error != "" {
			response.Error = frame.Error
		}
		if frame.Final {
			response.Final = true
			return worker.Modify(task.Delete()).OnSuccess(func(context.Context) error {
				stopWorker()
				return nil
			}), nil
		}
		if frame.ReplyQueue == "" {
			return nil, worker.FatalErrorf("non-final response for %q has no reply queue", session)
		}

		ack := Envelope{FrameControl: FrameControl{
			Session:    session,
			ReplyQueue: replyQueue,
		}}
		return worker.Modify(
			task.Delete(),
			entroq.InsertingInto(frame.ReplyQueue, entroq.WithValue(ack)),
		), nil
	}))
	if err := responseWorker.Run(workerCtx, worker.Watching(replyQueue)); err != nil {
		t.Fatalf("receive response for %q: %v", session, err)
	}
	return response
}
