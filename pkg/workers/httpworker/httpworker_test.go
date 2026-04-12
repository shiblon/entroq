package httpworker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

func TestWorker_run(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("Failed to create eq: %v", err)
	}
	defer eq.Close()

	// Mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) == "ping" {
			w.Header().Set("X-Test", "pong")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("pong"))
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	queue := "/test/http"

	// Insert a request task.
	req := &Request{
		Method: http.MethodPost,
		URL:    server.URL,
		Body:   []byte("ping"),
		Outbox: "/test/out",
	}
	if _, err := eq.Modify(ctx, entroq.InsertingInto(queue, entroq.WithValue(req))); err != nil {
		t.Fatalf("Failed to insert request task: %v", err)
	}

	// Run worker in background.
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, eq, Watching(queue))
	}()

	// Wait for response in outbox.
	task, err := eq.Claim(ctx, entroq.From("/test/out"), entroq.ClaimFor(time.Second))
	if err != nil {
		t.Fatalf("Failed to claim response task: %v", err)
	}

	resp, err := entroq.GetValue[Response](task)
	if err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status code mismatch: expected %d, got %d (Error: %q)", http.StatusOK, resp.StatusCode, resp.Error)
	}

	if string(resp.Body) != "pong" {
		t.Errorf("Body mismatch: expected %q, got %q", "pong", string(resp.Body))
	}

	if resp.Header["X-Test"][0] != "pong" {
		t.Errorf("Header mismatch: expected X-Test=pong, got %v", resp.Header["X-Test"])
	}

	if resp.Elapsed <= 0 {
		t.Errorf("Expected positive elapsed time, got %v", resp.Elapsed)
	}

	cancel()
	err = <-errCh
	if err != nil && !entroq.IsCanceled(err) {
		t.Errorf("Worker failed: %v", err)
	}
}

func TestWorker_timeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("Failed to create eq: %v", err)
	}
	defer eq.Close()

	// Slow mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	queue := "/test/timeout"
	req := &Request{
		URL:     server.URL,
		Timeout: 100 * time.Millisecond, // Very short timeout!
		Errbox:  "/test/err",
	}
	if _, err := eq.Modify(ctx, entroq.InsertingInto(queue, entroq.WithValue(req))); err != nil {
		t.Fatalf("Failed to insert request task: %v", err)
	}

	// Run worker
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, eq, Watching(queue))
	}()

	// Wait for error in errbox.
	task, err := eq.Claim(ctx, entroq.From("/test/err"), entroq.ClaimFor(time.Second))
	if err != nil {
		t.Fatalf("Failed to claim error task: %v", err)
	}

	resp, err := entroq.GetValue[Response](task)
	if err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if resp.Error == "" {
		t.Error("Expected error message in response, got empty")
	}

	cancel()
	err = <-errCh
	if err != nil && !entroq.IsCanceled(err) {
		t.Errorf("Worker failed: %v", err)
	}
}
