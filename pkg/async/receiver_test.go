package async

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/queues"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
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

type closeNotifyBody struct {
	once   sync.Once
	closed chan struct{}
}

func newCloseNotifyBody() *closeNotifyBody {
	return &closeNotifyBody{closed: make(chan struct{})}
}

func (b *closeNotifyBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *closeNotifyBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func (w *notifyingResponseWriter) Flush() {
	w.ResponseRecorder.Flush()
	select {
	case w.flushed <- struct{}{}:
	default:
	}
}

func TestSessionForcedQueueRotation(t *testing.T) {
	testSessionQueueRotation(t, laneTiming{
		lifetime:        3 * time.Second,
		piggybackBefore: 2 * time.Second,
		forceBefore:     time.Second,
	}, true)
}

func TestSessionDataPiggybacksQueueRotation(t *testing.T) {
	testSessionQueueRotation(t, laneTiming{
		lifetime:        6 * time.Second,
		piggybackBefore: 4 * time.Second,
		forceBefore:     time.Second,
	}, false)
}

func testSessionQueueRotation(t *testing.T, timing laneTiming, force bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
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

	const sessionID = "rotation-session"
	requestAck := newReceiveLane("/service", sessionID, "request-ack", time.Now(), timing)
	responseData := newReceiveLane("/service", sessionID, "response-data", time.Now(), timing)
	forceAt := responseData.forceAt()
	piggybackAt := responseData.collectAt.Add(-timing.piggybackBefore)
	sender := NewSender(eq, "", WithSenderRequestTimeout(10*time.Second))
	sender.laneTiming = timing
	w := &notifyingResponseWriter{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 8)}
	request := httptest.NewRequest(http.MethodGet, "http://service/events", nil)
	session := newSenderSession(sender, w, request, sessionID, requestAck, responseData, nil, time.Now())
	senderDone := make(chan error, 1)
	go func() { senderDone <- session.run(ctx) }()

	if _, err := eq.Modify(ctx, entroq.InsertingInto("/service/inbox", entroq.WithValue(Envelope{
		FrameControl:  FrameControl{Session: sessionID, ReplyQueue: requestAck.queue},
		ResponseQueue: responseData.queue,
		Method:        http.MethodGet,
		Path:          "/events",
		ProtocolMajor: 1,
	}))); err != nil {
		t.Fatalf("insert initial frame: %v", err)
	}

	waitForFlush(t, ctx, w.flushed, "initial response metadata")
	if force {
		if delay := time.Until(forceAt.Add(200 * time.Millisecond)); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				t.Fatal("timed out before forced rotation")
			}
		}
	} else {
		if delay := time.Until(piggybackAt.Add(200 * time.Millisecond)); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				t.Fatal("timed out before piggyback rotation")
			}
		}
	}

	if _, err := responseWriter.Write([]byte("data: first\n\n")); err != nil {
		t.Fatalf("write first body: %v", err)
	}
	waitForFlush(t, ctx, w.flushed, "first body after rotation threshold")
	if _, err := responseWriter.Write([]byte("data: second\n\n")); err != nil {
		t.Fatalf("write second body: %v", err)
	}
	waitForFlush(t, ctx, w.flushed, "second body through replacement workers")
	if err := responseWriter.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}

	select {
	case err := <-senderDone:
		if err != nil {
			t.Fatalf("sender session: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("sender session did not finish")
	}
	if got := w.Body.String(); got != "data: first\n\ndata: second\n\n" {
		t.Errorf("streaming body: got %q", got)
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

func waitForDocCount(t *testing.T, ctx context.Context, eq *entroq.EntroQ, namespace string, want int) []*entroq.Doc {
	t.Helper()
	for {
		docs, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: namespace})
		if err != nil {
			t.Fatalf("read docs from %q: %v", namespace, err)
		}
		if len(docs) == want {
			return docs
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("docs in %q: got %d, want %d", namespace, len(docs), want)
		}
	}
}

func waitForQueueSize(t *testing.T, ctx context.Context, eq *entroq.EntroQ, queue string, want int) {
	t.Helper()
	for {
		stats, err := eq.QueueStats(ctx, entroq.MatchExact(queue))
		if err != nil {
			t.Fatalf("read stats for %q: %v", queue, err)
		}
		got := 0
		if stat := stats[queue]; stat != nil {
			got = stat.Size
		}
		if got == want {
			return
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("queue %q size: got %d, want %d", queue, got, want)
		}
	}
}

func claimedResponseLanes(ctx context.Context, eq *entroq.EntroQ, prefix string) (int, error) {
	stats, err := eq.QueueStats(ctx, entroq.MatchPrefix(prefix+"/"))
	if err != nil {
		return 0, err
	}
	claimed := 0
	for queue, stat := range stats {
		if strings.HasSuffix(queue, "/response-ack") || strings.HasSuffix(queue, "/response-data") {
			claimed += stat.Claimed
		}
	}
	return claimed, nil
}

func waitForClaimedResponseLanes(t *testing.T, ctx context.Context, eq *entroq.EntroQ, prefix string, want int) {
	t.Helper()
	for {
		got, err := claimedResponseLanes(ctx, eq, prefix)
		if err != nil {
			t.Fatalf("read response lane stats under %q: %v", prefix, err)
		}
		if got == want {
			return
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("claimed response lanes under %q: got %d, want %d", prefix, got, want)
		}
	}
}

func TestPendingSenderInboxDemandIsCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("new EntroQ: %v", err)
	}
	defer eq.Close()

	const peerTimeout = 2 * time.Second
	sender := NewSender(eq, "",
		WithSenderDomainSuffix(".test"),
		WithSenderRequestTimeout(peerTimeout),
	)
	requestCtx, cancelRequest := context.WithCancel(ctx)
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		request := httptest.NewRequest(http.MethodGet, "http://service.test/pending", nil).WithContext(requestCtx)
		sender.ServeHTTP(httptest.NewRecorder(), request)
	}()

	waitForQueueSize(t, ctx, eq, "/service/inbox", 1)

	cancelRequest()
	select {
	case <-senderDone:
	case <-ctx.Done():
		t.Fatal("sender did not stop after caller cancellation")
	}
	waitForQueueSize(t, ctx, eq, "/service/inbox", 0)
}

func TestHeartbeatsKeepIdleSessionAlive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
			Header:     make(http.Header),
			Body:       responseReader,
			Request:    req,
		}, nil
	})}
	const peerTimeout = 300 * time.Millisecond
	receiverCtx, stopReceiver := context.WithCancel(ctx)
	receiver := NewReceiver(eq, "http://upstream",
		WithReceiverHTTPClient(client),
		WithReceiverRequestTimeout(peerTimeout),
	)
	receiverDone := make(chan error, 1)
	go func() { receiverDone <- receiver.Run(receiverCtx, "/service/inbox") }()

	writer := &notifyingResponseWriter{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 1)}
	sender := NewSender(eq, "",
		WithSenderDomainSuffix(".test"),
		WithSenderRequestTimeout(peerTimeout),
	)
	senderDone := make(chan error, 1)
	go func() {
		request := httptest.NewRequest(http.MethodGet, "http://service.test/idle", nil).WithContext(ctx)
		sender.ServeHTTP(writer, request)
		senderDone <- nil
	}()
	waitForFlush(t, ctx, writer.flushed, "response headers")
	waitForClaimedResponseLanes(t, ctx, eq, "/service", 1)

	timer := time.NewTimer(3 * peerTimeout)
	select {
	case <-senderDone:
		t.Fatal("healthy idle session ended despite heartbeat acknowledgements")
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal("timed out while observing healthy heartbeats")
	}
	// The GET request body has already half-closed. While the sender waits for a
	// server push, heartbeat turns keep exactly one response-direction token
	// claimed, which is the active-session autoscaling signal.
	waitForClaimedResponseLanes(t, ctx, eq, "/service", 1)
	if _, err := responseWriter.Write([]byte("still alive")); err != nil {
		t.Fatalf("write response after idle interval: %v", err)
	}
	if err := responseWriter.Close(); err != nil {
		t.Fatalf("close response: %v", err)
	}
	select {
	case <-senderDone:
		if writer.Body.String() != "still alive" {
			t.Fatalf("response after heartbeats: got %q", writer.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("session did not finish after idle interval")
	}
	waitForClaimedResponseLanes(t, ctx, eq, "/service", 0)
	waitForDocCount(t, ctx, eq, receiverSessionDocNamespace("/service"), 0)

	stopReceiver()
	select {
	case err := <-receiverDone:
		if err != nil {
			t.Fatalf("receiver: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("receiver did not stop")
	}
}

func TestSenderWorkersStopAtQueueGCDeadline(t *testing.T) {
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

	requestAck := newReceiveLane("/service", "expired-session", "request-ack", time.Now(), timing)
	responseData := newReceiveLane("/service", "expired-session", "response-data", time.Now(), timing)
	sender := NewSender(eq, "", WithSenderRequestTimeout(4*time.Second))
	sender.laneTiming = timing
	request := httptest.NewRequest(http.MethodGet, "http://service/events", nil)
	session := newSenderSession(sender, httptest.NewRecorder(), request, "expired-session", requestAck, responseData, nil, time.Now())
	if err := session.run(ctx); !errors.Is(err, errSenderQueueExpired) {
		t.Fatalf("sender error: got %v, want %v", err, errSenderQueueExpired)
	}
}

func TestSenderCancellationNotifiesLastRequestDataQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("new EntroQ: %v", err)
	}
	defer eq.Close()

	const sessionID = "canceled-session"
	requestAck := newReceiveLane("/service", sessionID, "request-ack", time.Now(), defaultLaneTiming)
	responseData := newReceiveLane("/service", sessionID, "response-data", time.Now(), defaultLaneTiming)
	peer := newReceiveLane("/service", sessionID, "request-data", time.Now(), defaultLaneTiming)
	sender := NewSender(eq, "")
	request := httptest.NewRequest(http.MethodPost, "http://service/work", strings.NewReader("pending"))
	session := newSenderSession(sender, httptest.NewRecorder(), request, sessionID, requestAck, responseData, nil, time.Now())
	if _, err := session.requestLanes.observePeer(peer.queue); err != nil {
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

func TestReceiverConcurrentSessionLifecycles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("new EntroQ: %v", err)
	}
	defer eq.Close()

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if _, err := io.ReadAll(req.Body); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(req.URL.Path)),
			Request:    req,
		}, nil
	})}
	receiverCtx, stopReceiver := context.WithCancel(ctx)
	receiver := NewReceiver(eq, "http://upstream", WithReceiverHTTPClient(client), WithReceiverConcurrency(4))
	receiverDone := make(chan error, 1)
	go func() { receiverDone <- receiver.Run(receiverCtx, "/service/inbox") }()

	sender := NewSender(eq, "", WithSenderDomainSuffix(".test"))
	const sessions = 32
	errCh := make(chan error, sessions)
	var group sync.WaitGroup
	for i := range sessions {
		group.Add(1)
		go func() {
			defer group.Done()
			request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://service.test/session-%02d", i), nil).WithContext(ctx)
			request.Host = "service.test"
			response := httptest.NewRecorder()
			sender.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				errCh <- fmt.Errorf("session %02d status: %d", i, response.Code)
				return
			}
			if got, want := response.Body.String(), fmt.Sprintf("/session-%02d", i); got != want {
				errCh <- fmt.Errorf("session %02d body: got %q, want %q", i, got, want)
			}
		}()
	}
	group.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	waitForDocCount(t, ctx, eq, receiverSessionDocNamespace("/service"), 0)

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

func TestSidecarsRestartAfterEntroQOutage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	eq, gate := newGatedEntroQ(t, ctx)
	var calls atomic.Int32
	firstStarted := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("recovered")),
			Request:    req,
		}, nil
	})}

	startReceiver := func() (context.CancelFunc, <-chan error) {
		receiverCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		receiver := NewReceiver(eq, "http://upstream", WithReceiverHTTPClient(client))
		go func() { done <- receiver.Run(receiverCtx, "/service/inbox") }()
		return stop, done
	}
	stopFirstReceiver, firstReceiverDone := startReceiver()
	defer stopFirstReceiver()

	sender := NewSender(eq, "", WithSenderDomainSuffix(".test"), WithSenderRequestTimeout(3*time.Second))
	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		sender.ServeHTTP(firstResponse, httptest.NewRequest(http.MethodGet, "http://service.test/first", nil).WithContext(ctx))
	}()
	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatal("first upstream exchange did not start")
	}

	gate.set(false)
	select {
	case err := <-firstReceiverDone:
		if err == nil || !entroq.IsUnavailable(err) {
			t.Fatalf("receiver outage error: got %v, want unavailable", err)
		}
	case <-ctx.Done():
		t.Fatal("receiver did not exit after EntroQ disappeared")
	}
	select {
	case <-firstDone:
		if firstResponse.Code < 500 {
			t.Fatalf("active request status: got %d, want failure", firstResponse.Code)
		}
	case <-ctx.Done():
		t.Fatal("active request did not fail after EntroQ disappeared")
	}

	gate.set(true)
	for {
		if _, err := eq.Modify(ctx, entroq.InsertingInto("/reconnect/gc=0", entroq.WithRawValue([]byte("{}")))); err == nil {
			break
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("EntroQ client did not reconnect")
		}
	}

	stopSecondReceiver, secondReceiverDone := startReceiver()
	secondResponse := httptest.NewRecorder()
	sender.ServeHTTP(secondResponse, httptest.NewRequest(http.MethodGet, "http://service.test/second", nil).WithContext(ctx))
	if secondResponse.Code != http.StatusOK || secondResponse.Body.String() != "recovered" {
		t.Fatalf("request after restart: status=%d body=%q", secondResponse.Code, secondResponse.Body.String())
	}
	stopSecondReceiver()
	select {
	case err := <-secondReceiverDone:
		if err != nil {
			t.Fatalf("restarted receiver: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("restarted receiver did not stop")
	}
}

func TestSenderCrashLeavesOnlyGCManagedSessionState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("new EntroQ: %v", err)
	}
	defer eq.Close()

	responseBody := newCloseNotifyBody()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       responseBody,
			Request:    req,
		}, nil
	})}
	receiverCtx, stopReceiver := context.WithCancel(ctx)
	receiver := NewReceiver(eq, "http://upstream",
		WithReceiverHTTPClient(client),
		WithReceiverRequestTimeout(600*time.Millisecond),
	)
	receiverDone := make(chan error, 1)
	go func() { receiverDone <- receiver.Run(receiverCtx, "/service/inbox") }()

	requestCtx, dropSender := context.WithCancel(ctx)
	writer := &notifyingResponseWriter{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 1)}
	sender := NewSender(eq, "", WithSenderDomainSuffix(".test"))
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		sender.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "http://service.test/stream", nil).WithContext(requestCtx))
	}()
	waitForFlush(t, ctx, writer.flushed, "response headers")

	// Losing the sender process drops its local connection context. The peer
	// may receive a best-effort cancellation, but it cannot rely on one.
	dropSender()
	select {
	case <-senderDone:
	case <-ctx.Done():
		t.Fatal("sender session did not stop after its connection disappeared")
	}
	select {
	case <-responseBody.closed:
	case <-ctx.Done():
		t.Fatal("receiver did not close the upstream body after unanswered heartbeats")
	}
	assertOnlyGCManagedSessionState(t, ctx, eq, "/service")

	stopReceiver()
	select {
	case err := <-receiverDone:
		if err != nil {
			t.Fatalf("receiver: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("receiver did not stop")
	}
}

func TestReceiverCrashLeavesOnlyGCManagedSessionState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("new EntroQ: %v", err)
	}
	defer eq.Close()

	upstreamStarted := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(upstreamStarted)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	receiverCtx, crashReceiver := context.WithCancel(ctx)
	receiver := NewReceiver(eq, "http://upstream", WithReceiverHTTPClient(client))
	receiverDone := make(chan error, 1)
	go func() { receiverDone <- receiver.Run(receiverCtx, "/service/inbox") }()

	sender := NewSender(eq, "", WithSenderDomainSuffix(".test"), WithSenderRequestTimeout(500*time.Millisecond))
	response := httptest.NewRecorder()
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		sender.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://service.test/stream", nil).WithContext(ctx))
	}()
	select {
	case <-upstreamStarted:
	case <-ctx.Done():
		t.Fatal("upstream exchange did not start")
	}

	crashReceiver()
	select {
	case err := <-receiverDone:
		if err != nil {
			t.Fatalf("receiver cancellation: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("receiver did not stop")
	}
	select {
	case <-senderDone:
		if response.Code < 500 {
			t.Fatalf("sender status after receiver crash: got %d, want a server error", response.Code)
		}
	case <-ctx.Done():
		t.Fatal("sender did not time out after receiver crash")
	}
	assertOnlyGCManagedSessionState(t, ctx, eq, "/service")
}

func assertOnlyGCManagedSessionState(t *testing.T, ctx context.Context, eq *entroq.EntroQ, prefix string) {
	t.Helper()
	docNS := receiverSessionDocNamespace(prefix)
	if _, present, err := queues.GCActivation(docNS); err != nil || !present {
		t.Fatalf("receiver session namespace is not GC-managed: namespace=%q present=%v err=%v", docNS, present, err)
	}
	if _, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: docNS}); err != nil {
		t.Fatalf("read abandoned receiver session docs from %q: %v", docNS, err)
	}
	// A terminal frame may commit concurrently with the simulated crash and
	// delete the receiver's doc. If it does not, the namespace marker above
	// guarantees that the abandoned doc remains eligible for collection.

	stats, err := eq.QueueStats(ctx, entroq.MatchPrefix(prefix+"/"))
	if err != nil {
		t.Fatalf("read abandoned session queues: %v", err)
	}
	for name, stat := range stats {
		if stat.Size == 0 {
			continue
		}
		if _, present, err := queues.GCActivation(name); err != nil || !present {
			t.Errorf("abandoned queue is not GC-managed: queue=%q present=%v err=%v", name, present, err)
		}
	}
}

type entroqGate struct {
	mu    sync.Mutex
	open  bool
	dial  eqtest.Dialer
	conns []net.Conn
}

func (g *entroqGate) set(open bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open = open
	if open {
		return
	}
	for _, connection := range g.conns {
		_ = connection.Close()
	}
	g.conns = nil
}

func (g *entroqGate) dialer() (net.Conn, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		return nil, fmt.Errorf("EntroQ unavailable")
	}
	connection, err := g.dial()
	if err != nil {
		return nil, err
	}
	g.conns = append(g.conns, connection)
	return connection, nil
}

func newGatedEntroQ(t *testing.T, ctx context.Context) (*entroq.EntroQ, *entroqGate) {
	t.Helper()
	stopService, dial, err := eqtest.StartService(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("start EntroQ service: %v", err)
	}
	t.Cleanup(stopService)

	gate := &entroqGate{open: true, dial: dial}
	eq, err := entroq.New(ctx, eqgrpc.Opener("bufnet",
		eqgrpc.WithNiladicDialer(gate.dialer),
		eqgrpc.WithInsecure(),
	))
	if err != nil {
		t.Fatalf("new gated EntroQ client: %v", err)
	}
	t.Cleanup(func() { _ = eq.Close() })
	return eq, gate
}
