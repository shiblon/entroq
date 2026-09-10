package async_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/async"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// echoResponse is the JSON shape returned by the echo upstream.
// ReqHeaders contains all non-standard request headers received, allowing
// tests to verify forwarding and filtering behavior.
type echoResponse struct {
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	Query      string              `json:"query"`
	Body       string              `json:"body"`
	ReqHeaders map[string][]string `json:"req_headers"`
}

func TestSidecarSSEStreamsBeforeUpstreamEOF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	firstFlushed := make(chan struct{})
	releaseUpstream := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseUpstream) }) }
	defer release()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream response writer does not support flushing")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		_, _ = io.WriteString(w, "data: hel")
		flusher.Flush()
		_, _ = io.WriteString(w, "lo\n\n")
		flusher.Flush()
		close(firstFlushed)

		select {
		case <-releaseUpstream:
		case <-ctx.Done():
			return
		}
		_, _ = io.WriteString(w, "event: done\ndata: goodbye\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	eq, stopEQ := mustStartEntroQ(ctx, t, eqmem.Opener())
	defer stopEQ()
	stopReceiver := mustStartReceivers(ctx, t, eq, "/events/svc", upstream.URL, 1)
	defer stopReceiver()

	sender := async.NewSender(eq, "",
		async.WithSenderDomainSuffix(".test"),
		async.WithSenderNamespace("events"),
		async.WithSenderRequestTimeout(5*time.Second),
	)
	senderServer := httptest.NewServer(sender)
	defer senderServer.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, senderServer.URL+"/stream", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "svc.test"
	req.Header.Set("Accept", "text/event-stream")
	resp, err := senderServer.Client().Do(req)
	if err != nil {
		t.Fatalf("open streaming response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type: got %q, want text/event-stream", got)
	}
	select {
	case <-firstFlushed:
	case <-ctx.Done():
		t.Fatal("upstream did not flush its first event")
	}

	type readResult struct {
		value string
		err   error
	}
	reader := bufio.NewReader(resp.Body)
	firstEvent := make(chan readResult, 1)
	go func() {
		line, readErr := reader.ReadString('\n')
		if readErr == nil {
			var separator string
			separator, readErr = reader.ReadString('\n')
			line += separator
		}
		firstEvent <- readResult{value: line, err: readErr}
	}()

	select {
	case got := <-firstEvent:
		if got.err != nil {
			t.Fatalf("read first event: %v", got.err)
		}
		if got.value != "data: hello\n\n" {
			t.Errorf("first event: got %q, want %q", got.value, "data: hello\n\n")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first SSE event did not arrive while upstream remained open")
	}

	release()
	remainder, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read remaining stream: %v", err)
	}
	if got, want := string(remainder), "event: done\ndata: goodbye\n\n"; got != want {
		t.Errorf("remaining stream: got %q, want %q", got, want)
	}
}

func TestSidecarHTTP2StreamsBothDirectionsConcurrently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	type requestResult struct {
		protocolMajor int
		body          string
		trailer       string
		err           error
	}
	requestDone := make(chan requestResult, 1)
	firstRequestSegment := make(chan struct{})
	upstream := httptest.NewServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first := make([]byte, len("hello"))
		if _, err := io.ReadFull(r.Body, first); err != nil {
			requestDone <- requestResult{err: err}
			return
		}
		close(firstRequestSegment)

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Trailer", "Grpc-Status")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "reply-one")
		w.(http.Flusher).Flush()

		rest, err := io.ReadAll(r.Body)
		requestDone <- requestResult{
			protocolMajor: r.ProtoMajor,
			body:          string(first) + string(rest),
			trailer:       r.Trailer.Get("X-Checksum"),
			err:           err,
		}
		_, _ = io.WriteString(w, "-reply-two")
		w.Header().Set("Grpc-Status", "0")
	}), &http2.Server{}))
	defer upstream.Close()

	eq, stopEQ := mustStartEntroQ(ctx, t, eqmem.Opener())
	defer stopEQ()
	stopReceiver := mustStartReceivers(ctx, t, eq, "/duplex/svc", upstream.URL, 1)
	defer stopReceiver()

	sender := async.NewSender(eq, "",
		async.WithSenderDomainSuffix(".test"),
		async.WithSenderNamespace("duplex"),
		async.WithSenderRequestTimeout(10*time.Second),
	)
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen for sender: %v", err)
	}
	senderDone := make(chan error, 1)
	go func() { senderDone <- sender.Serve(ctx, listener) }()
	defer func() {
		shutdownCtx, stopShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopShutdown()
		if err := sender.Close(shutdownCtx); err != nil {
			t.Errorf("close sender: %v", err)
		}
		if err := <-senderDone; err != nil {
			t.Errorf("serve sender: %v", err)
		}
	}()

	requestReader, requestWriter := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+listener.Addr().String()+"/stream", requestReader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "svc.test"
	req.Trailer = http.Header{"X-Checksum": nil}

	dialer := &net.Dialer{}
	client := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
	}}
	responseReady := make(chan struct {
		response *http.Response
		err      error
	}, 1)
	go func() {
		response, requestErr := client.Do(req)
		responseReady <- struct {
			response *http.Response
			err      error
		}{response, requestErr}
	}()

	if _, err := requestWriter.Write([]byte("hello")); err != nil {
		t.Fatalf("write first request segment: %v", err)
	}
	select {
	case <-firstRequestSegment:
	case <-ctx.Done():
		t.Fatal("upstream did not receive the first request segment")
	}

	var response *http.Response
	select {
	case result := <-responseReady:
		if result.err != nil {
			t.Fatalf("open response: %v", result.err)
		}
		response = result.response
	case <-ctx.Done():
		t.Fatal("response headers did not arrive while the request remained open")
	}
	defer response.Body.Close()
	firstResponse := make([]byte, len("reply-one"))
	if _, err := io.ReadFull(response.Body, firstResponse); err != nil {
		t.Fatalf("read first response segment: %v", err)
	}
	if got := string(firstResponse); got != "reply-one" {
		t.Fatalf("first response segment: got %q, want %q", got, "reply-one")
	}

	req.Trailer.Set("X-Checksum", "abc123")
	if _, err := requestWriter.Write([]byte(" world")); err != nil {
		t.Fatalf("write second request segment: %v", err)
	}
	if err := requestWriter.Close(); err != nil {
		t.Fatalf("close request body: %v", err)
	}

	remainder, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read remaining response: %v", err)
	}
	if got := string(remainder); got != "-reply-two" {
		t.Errorf("remaining response: got %q, want %q", got, "-reply-two")
	}
	if got := response.Trailer.Get("Grpc-Status"); got != "0" {
		t.Errorf("response trailer: got %q, want %q", got, "0")
	}

	select {
	case got := <-requestDone:
		if got.err != nil {
			t.Fatalf("read upstream request: %v", got.err)
		}
		if got.protocolMajor != 2 {
			t.Errorf("upstream protocol major: got %d, want 2", got.protocolMajor)
		}
		if got.body != "hello world" {
			t.Errorf("upstream request body: got %q, want %q", got.body, "hello world")
		}
		if got.trailer != "abc123" {
			t.Errorf("upstream request trailer: got %q, want %q", got.trailer, "abc123")
		}
	case <-ctx.Done():
		t.Fatal("upstream request did not complete")
	}
}

func TestSidecarDroppedUpstreamConnectionAbortsResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		connection, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack upstream connection: %v", err)
			return
		}
		_, _ = io.WriteString(buffered, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: 100\r\n\r\npartial")
		_ = buffered.Flush()
		_ = connection.Close()
	}))
	defer upstream.Close()

	eq, stopEQ := mustStartEntroQ(ctx, t, eqmem.Opener())
	defer stopEQ()
	stopReceiver := mustStartReceivers(ctx, t, eq, "/drops/svc", upstream.URL, 1)
	defer stopReceiver()

	sender := async.NewSender(eq, "",
		async.WithSenderDomainSuffix(".test"),
		async.WithSenderNamespace("drops"),
		async.WithSenderRequestTimeout(5*time.Second),
	)
	senderServer := httptest.NewServer(sender)
	defer senderServer.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, senderServer.URL+"/broken", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "svc.test"
	response, err := senderServer.Client().Do(req)
	if err == nil {
		defer response.Body.Close()
		body, readErr := io.ReadAll(response.Body)
		if readErr == nil {
			t.Fatalf("truncated response ended cleanly with body %q", body)
		}
	}

	docNamespace := "/drops/svc/connections/gc="
	for {
		docs, docsErr := eq.Docs(ctx, &entroq.DocQuery{Namespace: docNamespace})
		if docsErr != nil {
			t.Fatalf("read connection docs: %v", docsErr)
		}
		if len(docs) == 0 {
			break
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("connection doc remained after broken upstream response: %+v", docs)
		}
	}
}

// startEchoUpstream returns an httptest.Server that reflects the request
// back as echoResponse JSON. It also sets X-Response-Header and two values
// of X-Multi on the response for multi-value response header testing.
func startEchoUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Response-Header", "from-upstream")
		w.Header().Add("X-Multi", "first")
		w.Header().Add("X-Multi", "second")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(echoResponse{ //nolint:errcheck
			Method:     r.Method,
			Path:       r.URL.Path,
			Query:      r.URL.RawQuery,
			Body:       string(body),
			ReqHeaders: map[string][]string(r.Header),
		})
	}))
}

func TestSidecarE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	upstream := startEchoUpstream()
	defer upstream.Close()

	eq, stopEQ := mustStartEntroQ(ctx, t, eqmem.Opener())
	defer stopEQ()

	const (
		svc       = "svc"
		namespace = "payments"
		suffix    = ".test"
	)

	stopSvc := mustStartReceivers(ctx, t, eq, "/"+namespace+"/"+svc, upstream.URL, 2)
	defer stopSvc()

	sender := async.NewSender(eq, "",
		async.WithSenderDomainSuffix(suffix),
		async.WithSenderNamespace(namespace),
	)

	cases := []struct {
		name            string
		method          string
		url             string
		requestBody     string
		setupReq        func(*http.Request)
		wantPath        string
		wantQuery       string
		wantBody        string
		wantRespHeader  string
		wantRespMulti   []string
		checkReqHeaders func(*testing.T, map[string][]string)
	}{
		{
			name:     "simple GET",
			method:   http.MethodGet,
			url:      "http://svc.test/ping",
			wantPath: "/ping",
		},
		{
			name:     "nested path",
			method:   http.MethodGet,
			url:      "http://svc.test/api/v1/users",
			wantPath: "/api/v1/users",
		},
		{
			name:      "query parameters",
			method:    http.MethodGet,
			url:       "http://svc.test/search?q=hello&page=2",
			wantPath:  "/search",
			wantQuery: "q=hello&page=2",
		},
		{
			name:        "POST with body",
			method:      http.MethodPost,
			url:         "http://svc.test/submit",
			requestBody: `{"name":"test"}`,
			wantPath:    "/submit",
			wantBody:    `{"name":"test"}`,
		},
		{
			name:   "single custom request header forwarded",
			method: http.MethodGet,
			url:    "http://svc.test/check",
			setupReq: func(r *http.Request) {
				r.Header.Set("X-Custom-Header", "hello-from-caller")
			},
			wantPath: "/check",
			checkReqHeaders: func(t *testing.T, h map[string][]string) {
				t.Helper()
				if got := h["X-Custom-Header"]; len(got) != 1 || got[0] != "hello-from-caller" {
					t.Errorf("X-Custom-Header: got %v, want [hello-from-caller]", got)
				}
			},
		},
		{
			name:   "multi-value request header all values forwarded",
			method: http.MethodGet,
			url:    "http://svc.test/multi-req",
			setupReq: func(r *http.Request) {
				r.Header.Add("X-Multi", "alpha")
				r.Header.Add("X-Multi", "beta")
				r.Header.Add("X-Multi", "gamma")
			},
			wantPath: "/multi-req",
			checkReqHeaders: func(t *testing.T, h map[string][]string) {
				t.Helper()
				got := h["X-Multi"]
				if len(got) != 3 {
					t.Fatalf("X-Multi: got %v, want 3 values", got)
				}
				for i, want := range []string{"alpha", "beta", "gamma"} {
					if got[i] != want {
						t.Errorf("X-Multi[%d]: got %q, want %q", i, got[i], want)
					}
				}
			},
		},
		{
			name:   "hop-by-hop header not forwarded to upstream",
			method: http.MethodGet,
			url:    "http://svc.test/hop",
			setupReq: func(r *http.Request) {
				r.Header.Set("Connection", "keep-alive")
				r.Header.Set("X-Should-Pass", "yes")
			},
			wantPath: "/hop",
			checkReqHeaders: func(t *testing.T, h map[string][]string) {
				t.Helper()
				if _, present := h["Connection"]; present {
					t.Error("Connection header should have been filtered but was forwarded")
				}
				if got := h["X-Should-Pass"]; len(got) == 0 || got[0] != "yes" {
					t.Errorf("X-Should-Pass: got %v, want [yes]", got)
				}
			},
		},
		{
			name:           "single response header forwarded",
			method:         http.MethodGet,
			url:            "http://svc.test/resp-single",
			wantPath:       "/resp-single",
			wantRespHeader: "from-upstream",
		},
		{
			name:          "multi-value response header all values forwarded",
			method:        http.MethodGet,
			url:           "http://svc.test/resp-multi",
			wantPath:      "/resp-multi",
			wantRespMulti: []string{"first", "second"},
		},
		{
			name:     "namespace from host overrides flag",
			method:   http.MethodGet,
			url:      "http://payments.svc.test/namespaced",
			wantPath: "/namespaced",
		},
	}

	// Protocol upgrade rejection is separate: it returns before the queue is
	// touched, so it doesn't need a running receiver.
	upgradeCases := []struct {
		name       string
		setupReq   func(*http.Request)
		wantStatus int
	}{
		{
			name: "websocket upgrade rejected with 501",
			setupReq: func(r *http.Request) {
				r.Header.Set("Upgrade", "websocket")
			},
			wantStatus: http.StatusNotImplemented,
		},
	}
	for _, tc := range upgradeCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://svc.test/stream", nil).WithContext(ctx)
			tc.setupReq(req)
			w := httptest.NewRecorder()
			sender.ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d (body: %s)", w.Code, tc.wantStatus, w.Body)
			}
		})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.requestBody != "" {
				body = strings.NewReader(tc.requestBody)
			}
			req := httptest.NewRequest(tc.method, tc.url, body).WithContext(ctx)
			if tc.setupReq != nil {
				tc.setupReq(req)
			}

			w := httptest.NewRecorder()
			sender.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}

			if tc.wantRespHeader != "" {
				if got := w.Header().Get("X-Response-Header"); got != tc.wantRespHeader {
					t.Errorf("X-Response-Header: got %q, want %q", got, tc.wantRespHeader)
				}
			}

			if tc.wantRespMulti != nil {
				got := w.Header()["X-Multi"]
				if len(got) != len(tc.wantRespMulti) {
					t.Fatalf("X-Multi: got %v, want %v", got, tc.wantRespMulti)
				}
				for i, want := range tc.wantRespMulti {
					if got[i] != want {
						t.Errorf("X-Multi[%d]: got %q, want %q", i, got[i], want)
					}
				}
			}

			var echo echoResponse
			if err := json.NewDecoder(w.Body).Decode(&echo); err != nil {
				t.Fatalf("decode response: %v", err)
			}

			if echo.Method != tc.method {
				t.Errorf("method: got %q, want %q", echo.Method, tc.method)
			}
			if tc.wantPath != "" && echo.Path != tc.wantPath {
				t.Errorf("path: got %q, want %q", echo.Path, tc.wantPath)
			}
			if tc.wantQuery != "" && echo.Query != tc.wantQuery {
				t.Errorf("query: got %q, want %q", echo.Query, tc.wantQuery)
			}
			if tc.wantBody != "" && echo.Body != tc.wantBody {
				t.Errorf("body: got %q, want %q", echo.Body, tc.wantBody)
			}
			if tc.checkReqHeaders != nil {
				tc.checkReqHeaders(t, echo.ReqHeaders)
			}
		})
	}
}

// TestSidecarContentLength verifies that Content-Length set by the upstream
// is stripped from forwarded response headers and the body arrives intact.
// This is a regression test: blindly forwarding Content-Length while the body
// passes through JSON encoding caused an IncompleteRead on the receiving side.
func TestSidecarContentLength(t *testing.T) {
	const payload = `{"hello":"world","value":42}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := []byte(payload)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body) //nolint:errcheck
	}))
	defer upstream.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	eq, stopEQ := mustStartEntroQ(ctx, t, eqmem.Opener())
	defer stopEQ()

	stop := mustStartReceivers(ctx, t, eq, "/test/svc", upstream.URL, 1)
	defer stop()

	sender := async.NewSender(eq, "", async.WithSenderDomainSuffix(".local"), async.WithSenderNamespace("test"))

	req := httptest.NewRequest(http.MethodGet, "http://svc.local/data", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	sender.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if cl := w.Header().Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length should be stripped from forwarded response, got %q", cl)
	}
	// Compare as parsed JSON -- whitespace may differ after the round-trip but
	// the value must be structurally identical and complete (no IncompleteRead).
	var got, want any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response body not valid JSON: %v (body: %q)", err, w.Body)
	}
	if err := json.Unmarshal([]byte(payload), &want); err != nil {
		t.Fatalf("payload not valid JSON: %v", err)
	}
	gotBytes, _ := json.Marshal(got)
	wantBytes, _ := json.Marshal(want)
	if string(gotBytes) != string(wantBytes) {
		t.Errorf("body mismatch: got %s, want %s", gotBytes, wantBytes)
	}
}

// TestSidecarNamespaceRouting verifies that multi-label hosts produce the
// correct queue prefix, routing requests to the right inbox.
func TestSidecarNamespaceRouting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	upstream := startEchoUpstream()
	defer upstream.Close()

	eq, stopEQ := mustStartEntroQ(ctx, t, eqmem.Opener())
	defer stopEQ()

	stopA := mustStartReceivers(ctx, t, eq, "/ns1/alpha", upstream.URL, 1)
	defer stopA()
	stopB := mustStartReceivers(ctx, t, eq, "/ns2/beta", upstream.URL, 1)
	defer stopB()

	sender := async.NewSender(eq, "", async.WithSenderDomainSuffix(".local"))

	for _, tc := range []struct {
		url      string
		wantPath string
	}{
		{"http://ns1.alpha.local/foo", "/foo"},
		{"http://ns2.beta.local/bar", "/bar"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.url, nil).WithContext(ctx)
		w := httptest.NewRecorder()
		sender.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("%s: status %d: %s", tc.url, w.Code, w.Body)
			continue
		}
		var echo echoResponse
		if err := json.NewDecoder(w.Body).Decode(&echo); err != nil {
			t.Errorf("%s: decode: %v", tc.url, err)
			continue
		}
		if echo.Path != tc.wantPath {
			t.Errorf("%s: path got %q, want %q", tc.url, echo.Path, tc.wantPath)
		}
	}
}
