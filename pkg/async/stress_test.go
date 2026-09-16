//go:build eqlinkstress

package async

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/queues"
)

// TestBidirectionalSessionStress exercises the lifecycle shape expected in a
// mesh rather than EQLink throughput. It is intentionally excluded from the
// ordinary test suite; run it explicitly under the race detector:
//
//	go test -race -tags=eqlinkstress ./pkg/async \
//	  -run '^TestBidirectionalSessionStress$' -count=1
func TestBidirectionalSessionStress(t *testing.T) {
	// The timeout detects stalled sessions without turning race-detector
	// scheduling overhead into a liveness failure.
	const (
		sessions    = 96
		peerTimeout = 10 * time.Second
	)

	baselineGoroutines := runtime.NumGoroutine()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	upstream := httptest.NewServer(http.HandlerFunc(stressEchoHandler(t)))
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		upstream.Close()
		t.Fatalf("new EntroQ: %v", err)
	}

	receiverCtx, stopReceiver := context.WithCancel(ctx)
	receiver := NewReceiver(eq, upstream.URL,
		WithReceiverConcurrency(8),
		WithReceiverRequestTimeout(peerTimeout),
	)
	receiverDone := make(chan error, 1)
	go func() { receiverDone <- receiver.Run(receiverCtx, "/stress/service/inbox") }()

	sender := NewSender(eq, "",
		WithSenderDomainSuffix(".test"),
		WithSenderNamespace("stress"),
		WithSenderRequestTimeout(peerTimeout),
	)
	senderServer := httptest.NewServer(sender)

	errCh := make(chan error, sessions)
	var group sync.WaitGroup
	for session := range sessions {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := runStressSession(ctx, senderServer.Client(), senderServer.URL, session); err != nil {
				errCh <- err
			}
		}()
	}
	group.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	docNS := connectionDocNamespace("/stress/service")
	if _, present, err := queues.GCActivation(docNS); err != nil || !present {
		t.Errorf("connection docs are not GC-managed: namespace=%q present=%v err=%v", docNS, present, err)
	}
	docs, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: docNS})
	if err != nil {
		t.Errorf("list connection docs: %v", err)
	} else if len(docs) > sessions/5+1 {
		t.Errorf("connection docs after stress: got %d, want at most %d canceled sessions", len(docs), sessions/5+1)
	}
	stats, err := eq.QueueStats(ctx, entroq.MatchPrefix("/stress/service/"))
	if err != nil {
		t.Errorf("session queue stats: %v", err)
	}
	for queue, stat := range stats {
		if stat.Size == 0 {
			continue
		}
		if _, present, err := queues.GCActivation(queue); err != nil || !present {
			t.Errorf("nonempty residual queue is not GC-managed: queue=%q present=%v err=%v", queue, present, err)
		}
	}

	senderServer.CloseClientConnections()
	senderServer.Close()
	stopReceiver()
	select {
	case err := <-receiverDone:
		if err != nil {
			t.Errorf("receiver shutdown: %v", err)
		}
	case <-ctx.Done():
		t.Error("receiver did not stop")
	}
	upstream.CloseClientConnections()
	upstream.Close()
	if err := eq.Close(); err != nil {
		t.Errorf("close EntroQ: %v", err)
	}
	cancel()

	waitForGoroutineBudget(t, baselineGoroutines+16)
}

func stressEchoHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, request *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Errorf("enable upstream full duplex: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush upstream headers: %v", err)
			return
		}

		reader := bufio.NewReader(request.Body)
		for {
			segment, err := reader.ReadBytes('\n')
			if len(segment) > 0 {
				if _, writeErr := w.Write(segment); writeErr != nil {
					return
				}
				if flushErr := http.NewResponseController(w).Flush(); flushErr != nil {
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
					t.Logf("upstream request ended: %v", err)
				}
				return
			}
		}
	}
}

func runStressSession(ctx context.Context, client *http.Client, senderURL string, session int) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	request, err := http.NewRequestWithContext(sessionCtx, http.MethodPost, senderURL+"/duplex", reader)
	if err != nil {
		return fmt.Errorf("session %d request: %w", session, err)
	}
	request.Host = "service.test"

	type responseResult struct {
		response *http.Response
		err      error
	}
	responseReady := make(chan responseResult, 1)
	go func() {
		response, requestErr := client.Do(request)
		responseReady <- responseResult{response: response, err: requestErr}
	}()

	random := rand.New(rand.NewSource(int64(session) + 1))
	segments := 2 + random.Intn(7)
	var want bytes.Buffer
	for segment := range segments {
		size := 1 + random.Intn(32<<10)
		if session%24 == 0 && segment == 1 {
			size = streamReadBufferSize + 17
		}
		prefix := fmt.Sprintf("session=%03d segment=%02d ", session, segment)
		payload := bytes.Repeat([]byte{'a' + byte(session%26)}, size)
		frame := append(append([]byte(prefix), payload...), '\n')
		if _, err := want.Write(frame); err != nil {
			return fmt.Errorf("session %d build expectation: %w", session, err)
		}
		if _, err := writer.Write(frame); err != nil {
			if session%5 == 0 && errors.Is(err, io.ErrClosedPipe) {
				return nil
			}
			return fmt.Errorf("session %d write segment %d: %w", session, segment, err)
		}
		if session%5 == 0 && segment == 0 {
			cancel()
			_ = writer.CloseWithError(context.Canceled)
			select {
			case result := <-responseReady:
				if result.response != nil {
					_ = result.response.Body.Close()
				}
			case <-time.After(4 * time.Second):
				return fmt.Errorf("session %d cancellation did not release HTTP client", session)
			}
			return nil
		}
		if random.Intn(3) == 0 {
			time.Sleep(time.Duration(random.Intn(3)+1) * time.Millisecond)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("session %d close request: %w", session, err)
	}

	var result responseResult
	select {
	case result = <-responseReady:
	case <-ctx.Done():
		return fmt.Errorf("session %d response headers: %w", session, ctx.Err())
	}
	if result.err != nil {
		return fmt.Errorf("session %d response: %w", session, result.err)
	}
	defer result.response.Body.Close()
	if result.response.StatusCode != http.StatusOK {
		return fmt.Errorf("session %d status: got %d", session, result.response.StatusCode)
	}
	got, err := io.ReadAll(result.response.Body)
	if err != nil {
		return fmt.Errorf("session %d read response: %w", session, err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		return fmt.Errorf("session %d response mismatch: got %d bytes, want %d", session, len(got), want.Len())
	}
	return nil
}

func waitForGoroutineBudget(t *testing.T, maximum int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		if current := runtime.NumGoroutine(); current <= maximum {
			return
		} else if time.Now().After(deadline) {
			t.Errorf("goroutines after stress: got %d, want at most %d", current, maximum)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
