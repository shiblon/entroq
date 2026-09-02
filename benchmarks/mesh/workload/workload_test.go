package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiblon/entroq/pkg/authn"
	"github.com/shiblon/entroq/pkg/authz"
)

type stubAuthenticator struct {
	principal   *authn.VerifiedPrincipal
	err         error
	credentials *authn.Credentials
	calls       int
}

func (s *stubAuthenticator) Authenticate(_ context.Context, credentials *authn.Credentials) (*authn.VerifiedPrincipal, error) {
	s.calls++
	s.credentials = credentials
	return s.principal, s.err
}

type stubAuthorizer struct {
	err error
	req *authz.Request
}

func (s *stubAuthorizer) Authorize(_ context.Context, req *authz.Request) error {
	s.req = req
	return s.err
}

func TestRunPhase(t *testing.T) {
	server := httptest.NewServer(serveHandler(serveConfig{MaxBody: 1024}))
	defer server.Close()

	cfg := loadConfig{
		URL:            server.URL + "/work",
		AuthzStrategy:  "none",
		AuthzProfile:   "none",
		Concurrency:    2,
		RequestTimeout: time.Second,
		ExpectedStatus: http.StatusOK,
	}
	result := runPhase(server.Client(), cfg, makePayload(64), 50*time.Millisecond)
	if result.completed == 0 {
		t.Fatal("no requests completed")
	}
	if result.failures != 0 || result.invalid != 0 {
		t.Fatalf("failures=%d invalid=%d examples=%v", result.failures, result.invalid, result.examples)
	}
	if len(result.latencies) != int(result.completed) {
		t.Fatalf("latencies=%d completed=%d", len(result.latencies), result.completed)
	}
}

func TestRunPhasePaced(t *testing.T) {
	server := httptest.NewServer(serveHandler(serveConfig{MaxBody: 1024}))
	defer server.Close()

	cfg := loadConfig{
		URL:            server.URL + "/work",
		AuthzStrategy:  "none",
		AuthzProfile:   "none",
		Concurrency:    4,
		TargetRPS:      20,
		RequestTimeout: time.Second,
		ExpectedStatus: http.StatusOK,
	}
	result := runPhase(server.Client(), cfg, makePayload(64), 250*time.Millisecond)
	if result.completed < 3 || result.completed > 6 {
		t.Fatalf("completed=%d, want approximately 5 paced requests", result.completed)
	}
	if result.failures != 0 || result.invalid != 0 {
		t.Fatalf("failures=%d invalid=%d examples=%v", result.failures, result.invalid, result.examples)
	}
}

func TestMakePayload(t *testing.T) {
	for _, size := range []int{1, 2, 64, 1024} {
		payload := makePayload(size)
		if len(payload) != size {
			t.Fatalf("size=%d: payload bytes=%d", size, len(payload))
		}
		if !json.Valid(payload) {
			t.Fatalf("size=%d: invalid JSON %q", size, payload)
		}
	}
}

func TestServeHandlerRejectsLargeBody(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/work", strings.NewReader("too large"))
	response := httptest.NewRecorder()
	serveHandler(serveConfig{MaxBody: 3}).ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestServeHandlerForwardsWork(t *testing.T) {
	var gotHost string
	leaf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(body)
	}))
	defer leaf.Close()

	relay := serveHandler(serveConfig{
		MaxBody:      1024,
		UpstreamURL:  leaf.URL,
		UpstreamHost: "leaf.localhost",
		Client:       leaf.Client(),
	})
	request := httptest.NewRequest(http.MethodPost, "/work", strings.NewReader(`{"mesh":"two-hop"}`))
	response := httptest.NewRecorder()
	relay.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusOK)
	}
	if gotHost != "leaf.localhost" {
		t.Fatalf("host=%q, want leaf.localhost", gotHost)
	}
	if got := response.Body.String(); got != `{"mesh":"two-hop"}` {
		t.Fatalf("body=%q", got)
	}
}

func TestAuthProxyAuthorizesServiceThenForwards(t *testing.T) {
	principal := &authn.VerifiedPrincipal{Subject: "system:serviceaccount:mesh-bench:gateway"}
	authnStub := &stubAuthenticator{principal: principal}
	az := new(stubAuthorizer)
	var upstreamCalls int
	handler := authProxyHandler(authProxyConfig{
		Namespace:     "mesh-bench",
		DomainSuffix:  ".localhost",
		Service:       "leaf",
		Credentials:   authn.NewHeaderCredentials("Bearer test-token"),
		Authenticator: authnStub,
		Authorizer:    az,
		Upstream: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			upstreamCalls++
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`"ok"`))
		}),
	})
	request := httptest.NewRequest(http.MethodPost, "http://leaf.localhost/work", strings.NewReader(`"ok"`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || upstreamCalls != 1 {
		t.Fatalf("status=%d upstream calls=%d", response.Code, upstreamCalls)
	}
	if authnStub.calls != 1 || authnStub.credentials.Scheme != "Bearer" || authnStub.credentials.Token != "test-token" {
		t.Fatalf("authentication = calls %d, credentials %+v", authnStub.calls, authnStub.credentials)
	}
	if az.req == nil || az.req.Principal != principal {
		t.Fatalf("authorization request = %+v", az.req)
	}
	if len(az.req.Queues) != 1 || az.req.Queues[0].Exact != "/mesh-bench/leaf/inbox" ||
		len(az.req.Queues[0].Actions) != 1 || az.req.Queues[0].Actions[0] != authz.Insert {
		t.Fatalf("authorization queues = %+v", az.req.Queues)
	}
}

func TestAuthProxyDenialDoesNotReachService(t *testing.T) {
	authnStub := &stubAuthenticator{principal: &authn.VerifiedPrincipal{
		Subject: "system:serviceaccount:mesh-bench:gateway",
	}}
	az := &stubAuthorizer{err: errors.New("denied")}
	upstreamCalled := false
	handler := authProxyHandler(authProxyConfig{
		Namespace:     "mesh-bench",
		DomainSuffix:  ".localhost",
		Service:       "leaf",
		Credentials:   authn.NewHeaderCredentials("Bearer test-token"),
		Authenticator: authnStub,
		Authorizer:    az,
		Upstream: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			upstreamCalled = true
		}),
	})
	request := httptest.NewRequest(http.MethodPost, "http://denied.localhost/work", strings.NewReader(`"no"`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusForbidden)
	}
	if upstreamCalled {
		t.Fatal("denied request reached the upstream service")
	}
	if az.req.Queues[0].Exact != "/mesh-bench/denied/inbox" {
		t.Fatalf("denied queue = %q", az.req.Queues[0].Exact)
	}
}

func TestAuthProxyAuthenticationFailureDoesNotReachOPAOrService(t *testing.T) {
	authnStub := &stubAuthenticator{err: authn.InvalidError("JWT verification failed", nil)}
	az := new(stubAuthorizer)
	upstreamCalled := false
	handler := authProxyHandler(authProxyConfig{
		Namespace:     "mesh-bench",
		DomainSuffix:  ".localhost",
		Service:       "leaf",
		Credentials:   authn.NewHeaderCredentials("Bearer invalid-token"),
		Authenticator: authnStub,
		Authorizer:    az,
		Upstream: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			upstreamCalled = true
		}),
	})
	request := httptest.NewRequest(http.MethodPost, "http://leaf.localhost/work", strings.NewReader(`"no"`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusUnauthorized)
	}
	if az.req != nil {
		t.Fatal("unauthenticated request reached OPA")
	}
	if upstreamCalled {
		t.Fatal("unauthenticated request reached the upstream service")
	}
}

func TestInstallOPAAllowAll(t *testing.T) {
	var gotMethod, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := installOPAAllowAll([]string{"--url=" + server.URL, "--timeout=1s"}); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut || gotBody != allowAllPolicy {
		t.Fatalf("request = %s %q", gotMethod, gotBody)
	}
}

func TestMetricSampler(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "sender_handled_total 7\n")
	}))
	defer server.Close()

	sampler := newMetricSampler([]metricSpec{{Name: "sender", URL: server.URL}}, time.Hour, time.Now())
	sampler.start()
	snapshots := sampler.stop()
	if len(snapshots) != 2 {
		t.Fatalf("snapshots=%d, want 2", len(snapshots))
	}
	for _, snapshot := range snapshots {
		if snapshot.Error != "" || !strings.Contains(snapshot.Body, "sender_handled_total") {
			t.Fatalf("snapshot=%+v", snapshot)
		}
	}
}

func TestMetricSamplerIgnoresShutdownCancellation(t *testing.T) {
	secondStarted := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 2 {
			close(secondStarted)
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "sender_handled_total 7\n")
	}))
	defer server.Close()

	sampler := newMetricSampler([]metricSpec{{Name: "sender", URL: server.URL}}, time.Millisecond, time.Now())
	sampler.start()
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("periodic scrape did not start")
	}
	snapshots := sampler.stop()
	for _, snapshot := range snapshots {
		if snapshot.Error != "" {
			t.Fatalf("shutdown cancellation recorded as scrape error: %+v", snapshot)
		}
	}
}

func TestSummarizeLatencies(t *testing.T) {
	values := []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond, 4 * time.Millisecond}
	summary := summarizeLatencies(values)
	if summary.P50 != 2 || summary.P95 != 4 || summary.P99 != 4 || summary.Max != 4 {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestObservedPositiveMetricRequiresNamedSource(t *testing.T) {
	results := []sampleResult{{
		Config: loadConfig{Mode: "mesh2"},
		MetricSnapshots: []metricSnapshot{
			{Source: "gateway", Body: "sender_handled_total 9\n"},
			{Source: "relay", Body: "# HELP sender_handled_total handled\nsender_handled_total 0\n"},
		},
	}}
	if !observedPositiveMetric(results, "mesh2", "gateway", "sender_handled_total") {
		t.Fatal("positive gateway counter was not observed")
	}
	if observedPositiveMetric(results, "mesh2", "relay", "sender_handled_total") {
		t.Fatal("zero relay counter was treated as positive")
	}
	if observedPositiveMetric(results, "mesh2", "leaf", "sender_handled_total") {
		t.Fatal("counter from another source was accepted")
	}
}

func TestOPADecisionCountUsesPostDataHandler(t *testing.T) {
	result := sampleResult{MetricSnapshots: []metricSnapshot{
		{Source: "opa", Body: "http_request_duration_seconds_count{code=\"200\",handler=\"v1/data\",method=\"post\"} 10\nhttp_request_duration_seconds_count{code=\"204\",handler=\"v1/policies\",method=\"put\"} 2\n"},
		{Source: "opa", Body: "http_request_duration_seconds_count{code=\"200\",handler=\"v1/data\",method=\"post\"} 25\nhttp_request_duration_seconds_count{code=\"204\",handler=\"v1/policies\",method=\"put\"} 9\n"},
	}}
	if count, ok := opaDecisionCount(result); !ok || count != 15 {
		t.Fatalf("OPA decisions = %v, %v; want 15, true", count, ok)
	}
}

func TestPairedRatiosMatchSampleNumbers(t *testing.T) {
	numerators := []sampleResult{
		{Config: loadConfig{Sample: 2}, Throughput: 40},
		{Config: loadConfig{Sample: 1}, Throughput: 20},
	}
	denominators := []sampleResult{
		{Config: loadConfig{Sample: 1}, Throughput: 10},
		{Config: loadConfig{Sample: 2}, Throughput: 10},
	}
	ratios := pairedRatios(numerators, denominators,
		func(r sampleResult) float64 { return r.Throughput },
		func(r sampleResult) float64 { return r.Throughput })
	if len(ratios) != 2 || ratios[0] != 2 || ratios[1] != 4 {
		t.Fatalf("ratios = %v, want [2 4]", ratios)
	}
}
