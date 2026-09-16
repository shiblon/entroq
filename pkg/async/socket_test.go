package async

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestResponseSocketStreamsRequestAndResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	requestSeen := make(chan string, 1)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		requestSeen <- string(body) + ":" + req.Trailer.Get("X-Checksum")
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header:     http.Header{"Content-Type": []string{"application/octet-stream"}},
			Trailer:    http.Header{"Grpc-Status": nil},
			Body:       io.NopCloser(strings.NewReader("response bytes")),
			Request:    req,
		}, nil
	})}

	socket := newResponseSocket()
	done := make(chan error, 1)
	go func() { done <- socket.run(ctx, client, "http://upstream") }()

	if err := socket.open(ctx, Envelope{
		FrameControl:  FrameControl{Session: "session"},
		Method:        http.MethodPost,
		Path:          "/work",
		ProtocolMajor: 2,
		ContentLength: -1,
		TrailerKeys:   []string{"X-Checksum"},
	}); err != nil {
		t.Fatalf("open socket: %v", err)
	}
	if err := socket.write(ctx, bodyEvent{body: []byte("request ")}); err != nil {
		t.Fatalf("write first request segment: %v", err)
	}
	if err := socket.write(ctx, bodyEvent{
		body:     []byte("bytes"),
		trailers: http.Header{"X-Checksum": []string{"abc123"}},
		end:      true,
	}); err != nil {
		t.Fatalf("write terminal request segment: %v", err)
	}

	metadata, _, err := socket.nextBefore(ctx, time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("read response metadata: %v", err)
	}
	if metadata.statusCode != http.StatusAccepted {
		t.Errorf("status: got %d, want %d", metadata.statusCode, http.StatusAccepted)
	}
	if len(metadata.trailerKeys) != 1 || metadata.trailerKeys[0] != "Grpc-Status" {
		t.Errorf("trailer keys: got %v", metadata.trailerKeys)
	}
	var responseBody []byte
	for {
		event, _, err := socket.nextBefore(ctx, time.Now().Add(time.Second))
		if err != nil {
			t.Fatalf("read response body: %v", err)
		}
		responseBody = append(responseBody, event.body...)
		if event.end {
			break
		}
	}
	if got := string(responseBody); got != "response bytes" {
		t.Errorf("response body: got %q, want %q", got, "response bytes")
	}

	select {
	case got := <-requestSeen:
		if got != "request bytes:abc123" {
			t.Errorf("upstream request: got %q", got)
		}
	case <-ctx.Done():
		t.Fatal("upstream request was not observed")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("socket run: %v", err)
	}
}

func TestResponseSocketWriteCancellationUnblocksPipe(t *testing.T) {
	testCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()

	ctx, cancel := context.WithCancel(testCtx)
	reader, writer := io.Pipe()
	defer reader.Close()

	socket := newResponseSocket()
	done := make(chan error, 1)
	go func() {
		done <- socket.writeRequest(ctx, &http.Request{}, writer)
	}()

	deliveryDone := make(chan error, 1)
	select {
	case socket.deliveries <- socketDelivery{
		event: bodyEvent{body: []byte("blocked")},
		done:  deliveryDone,
	}:
	case <-testCtx.Done():
		t.Fatal("request writer did not accept the delivery")
	}
	cancel()

	select {
	case err := <-deliveryDone:
		if err == nil {
			t.Fatal("blocked delivery succeeded after cancellation")
		}
	case <-testCtx.Done():
		t.Fatal("request delivery remained blocked after cancellation")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("request writer: %v", err)
		}
	case <-testCtx.Done():
		t.Fatal("request writer did not stop after cancellation")
	}

	buffer := make([]byte, 1)
	if _, err := reader.Read(buffer); !errors.Is(err, context.Canceled) {
		t.Fatalf("pipe close error: got %v, want %v", err, context.Canceled)
	}
}

func TestResponseSocketReceivesBufferedEventBeforeFutureDeadline(t *testing.T) {
	socket := newResponseSocket()
	want := socketEvent{body: []byte("ready")}
	socket.events <- want

	got, forced, err := socket.nextBefore(context.Background(), time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("next before: %v", err)
	}
	if forced {
		t.Fatal("future deadline won over an already-buffered socket event")
	}
	if string(got.body) != string(want.body) {
		t.Errorf("body: got %q, want %q", got.body, want.body)
	}
}

func TestResponseSocketDeadlineExpiresWithoutEvent(t *testing.T) {
	socket := newResponseSocket()

	_, forced, err := socket.nextBefore(context.Background(), time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("next before: %v", err)
	}
	if !forced {
		t.Fatal("expired deadline did not force progress")
	}
}

func TestResponseSocketNextBeforeCancellation(t *testing.T) {
	socket := newResponseSocket()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, forced, err := socket.nextBefore(ctx, time.Now().Add(time.Hour))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("next before error: got %v, want %v", err, context.Canceled)
	}
	if forced {
		t.Fatal("cancellation reported a forced deadline")
	}
}
