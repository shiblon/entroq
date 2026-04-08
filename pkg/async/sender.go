package async

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/shiblon/entroq"
)

const defaultRequestTimeout = 30 * time.Second

// Sender is an http.Handler that accepts outgoing HTTP calls from a local
// service and routes them through EntroQ task queues. Each request is
// translated into a task on the target queue, and the sender blocks waiting
// for a response task to appear on an ephemeral per-request response queue.
//
// Path routing: the first path segment names the target queue; the rest is
// forwarded as the request path. For example, a request to /svc-b/process
// enqueues a task on queue "svc-b" with forwarded path "/process".
type Sender struct {
	eq             *entroq.EntroQ
	myQueue        string
	requestTimeout time.Duration
	addr           string

	handled  atomic.Int64
	inFlight atomic.Int64
}

// SenderOption configures a Sender.
type SenderOption func(*Sender)

// WithRequestTimeout sets how long ServeHTTP waits for a response task before
// returning 504. Also used as the exp= value in the response queue name.
// Defaults to 30 seconds.
func WithRequestTimeout(d time.Duration) SenderOption {
	return func(s *Sender) {
		s.requestTimeout = d
	}
}

// NewSender creates a Sender that listens on addr and uses myQueue as the
// namespace for response and cleanup queues.
func NewSender(eq *entroq.EntroQ, addr, myQueue string, opts ...SenderOption) *Sender {
	s := &Sender{
		eq:             eq,
		addr:           addr,
		myQueue:        myQueue,
		requestTimeout: defaultRequestTimeout,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

const statusInterval = 10 * time.Minute

// Run starts the HTTP server on the address provided to NewSender and blocks
// until ctx is cancelled.
func (s *Sender) Run(ctx context.Context) error {
	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("sender listen: %w", err)
	}
	return s.Serve(ctx, lis)
}

// Serve starts the HTTP server on an already-open listener and blocks until
// ctx is cancelled. Useful when the caller needs to know the bound address
// before starting (e.g. when using ":0" for a random port in tests).
func (s *Sender) Serve(ctx context.Context, lis net.Listener) error {
	srv := &http.Server{Handler: s}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	go func() {
		t := time.NewTicker(statusInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				log.Printf("Still happy: sender %s: %d handled, %d in-flight",
					s.myQueue, s.handled.Load(), s.inFlight.Load())
			}
		}
	}()
	if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("sender serve: %w", err)
	}
	return nil
}

// ServeHTTP handles an outgoing request from the local service. It translates
// it into an Envelope task, atomically enqueues the task and a cleanup task,
// then blocks until a Response task arrives on the response queue.
func (s *Sender) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.inFlight.Add(1)
	defer func() {
		s.inFlight.Add(-1)
		s.handled.Add(1)
	}()

	ctx := r.Context()

	targetQueue, forwardPath, err := parsePath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// TODO: add MaxBodySize option and cap with http.MaxBytesReader to avoid
	// memory pressure from large payloads. Return 413 when exceeded.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read request body: %v", err), http.StatusBadRequest)
		return
	}

	headers := make(map[string]string, len(r.Header))
	for k := range r.Header {
		headers[k] = r.Header.Get(k)
	}

	expiry := time.Now().Add(s.requestTimeout)
	responseQueue := path.Join(s.myQueue, "response",
		fmt.Sprintf("exp=%d", expiry.Unix()),
		entroq.Hex16Generator())

	env := Envelope{
		Method:        r.Method,
		Path:          forwardPath,
		Headers:       headers,
		Body:          json.RawMessage(body),
		ResponseQueue: responseQueue,
	}
	envValue, err := json.Marshal(env)
	if err != nil {
		http.Error(w, fmt.Sprintf("marshal envelope: %v", err), http.StatusInternalServerError)
		return
	}

	if _, _, err := s.eq.Modify(ctx,
		entroq.InsertingInto(targetQueue, entroq.WithRawValue(envValue)),
	); err != nil {
		http.Error(w, fmt.Sprintf("enqueue task: %v", err), http.StatusBadGateway)
		return
	}

	// Block until the response arrives, the request timeout elapses, or the
	// caller disconnects. WithTimeout derives from ctx so whichever deadline
	// is sooner wins.
	claimCtx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()
	task, err := s.eq.Claim(claimCtx, entroq.From(responseQueue))
	if err != nil {
		log.Printf("sender await response on %s: %v", responseQueue, err)
		http.Error(w, fmt.Sprintf("await response: %v", err), http.StatusGatewayTimeout)
		return
	}
	defer s.deleteTask(ctx, task)

	var resp Response
	if err := json.Unmarshal(task.Value, &resp); err != nil {
		http.Error(w, fmt.Sprintf("unmarshal response: %v", err), http.StatusBadGateway)
		return
	}

	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(resp.Body); err != nil {
		log.Printf("sender write response: %v", err)
	}
}

// deleteTask removes a task from EntroQ, logging on failure. Used for
// response queue cleanup after the response has been read.
func (s *Sender) deleteTask(ctx context.Context, task *entroq.Task) {
	if _, _, err := s.eq.Modify(ctx, task.Delete()); err != nil {
		log.Printf("sender delete task %v: %v (GC will clean up)", task.ID, err)
	}
}

// parsePath splits an incoming path into the target queue name and the
// forwarded request path. The first non-empty path segment is the queue name;
// the remainder (with leading slash) is the forwarded path.
//
// "/svc-b/process" -> ("svc-b", "/process", nil)
// "/svc-b"         -> ("svc-b", "/",        nil)
//
// TODO: first-segment routing doesn't compose with hierarchical queue names
// (e.g. /ns=org/svc-b). A future convention (double-slash delimiter, header,
// etc.) could support namespaced targets, which would be useful for authz groups.
func parsePath(path string) (queue, forwardPath string, err error) {
	path = strings.TrimPrefix(path, "/")
	queue, rest, _ := strings.Cut(path, "/")
	if queue == "" {
		return "", "", fmt.Errorf("path must begin with a target queue name")
	}
	return queue, "/" + rest, nil
}
