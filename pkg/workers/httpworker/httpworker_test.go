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
	"github.com/shiblon/entroq/pkg/worker"
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

// TestWorkTimeoutRetriesTheRequest: a request cut off because the worker's
// work timeout ran out got no answer, so the task is retried, its attempt
// counted, rather than moved to the error box. The request's own Timeout
// (TestWorker_timeout) is an answer, and does go there.
func TestWorkTimeoutRetriesTheRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("Failed to create eq: %v", err)
	}
	defer eq.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer server.Close()

	const queue = "/test/worktimeout"
	req := &Request{URL: server.URL, Errbox: "/test/worktimeout/err"}
	if _, err := eq.Modify(ctx, entroq.InsertingInto(queue, entroq.WithValue(req))); err != nil {
		t.Fatalf("Failed to insert request task: %v", err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = Run(runCtx, eq, Watching(queue),
			WithRunOption(worker.WithWorkTimeout(200*time.Millisecond)),
			WithRunOption(worker.WithBaseRetryDelay(time.Hour)),
		)
	}()

	for {
		tasks, err := eq.Tasks(ctx, queue)
		if err != nil {
			t.Fatalf("Tasks: %v", err)
		}
		if len(tasks) == 1 && tasks[0].Attempt == 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the timed-out request's task was never retried: %v", tasks)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if failed, _ := eq.Tasks(ctx, "/test/worktimeout/err"); len(failed) != 0 {
		t.Errorf("a timed-out request landed in the error box: %v", failed)
	}
}
