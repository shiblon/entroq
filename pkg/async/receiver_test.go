package async

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
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

	const (
		session    = "session-1"
		replyQueue = "/service/replies/session-1"
	)
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
	responseTask, err := eq.Claim(ctx, entroq.From(replyQueue))
	if err != nil {
		t.Fatalf("claim response: %v", err)
	}
	response, err := entroq.GetValue[Response](responseTask)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Session != session || !response.Final {
		t.Errorf("terminal response control: got session=%q final=%v", response.Session, response.Final)
	}
	if got := string(response.Body); got != "stream complete" {
		t.Errorf("response body: got %q", got)
	}
	if _, err := eq.Modify(ctx, responseTask.Delete()); err != nil {
		t.Fatalf("delete response: %v", err)
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
		replyQueue := "/service/replies/" + session
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
	for range sessions {
		task, err := eq.Claim(ctx, entroq.From(replyQueues...))
		if err != nil {
			t.Fatalf("claim response: %v", err)
		}
		response, err := entroq.GetValue[Response](task)
		if err != nil {
			t.Fatalf("decode response: %v", err)
		}
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
		if _, err := eq.Modify(ctx, task.Delete()); err != nil {
			t.Fatalf("delete response: %v", err)
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
