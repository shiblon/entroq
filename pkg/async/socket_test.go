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

func TestOpenUpstreamResponseRetriesBufferedRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read attempt %d body: %v", attempts, err)
		}
		if got, want := string(body), "request bytes"; got != want {
			t.Fatalf("attempt %d body: got %q, want %q", attempts, got, want)
		}
		if attempts < forwardMaxAttempts {
			return nil, errors.New("temporary connection failure")
		}
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("response")),
			Request:    req,
		}, nil
	})}

	response, err := openUpstreamResponse(ctx, client, "http://upstream", Envelope{
		Method: http.MethodPost,
		Path:   "/work",
		Body:   []byte("request bytes"),
	})
	if err != nil {
		t.Fatalf("open response: %v", err)
	}
	defer response.Body.Close()
	if attempts != forwardMaxAttempts {
		t.Errorf("attempts: got %d, want %d", attempts, forwardMaxAttempts)
	}
}

func TestResponseSocketBufferedEventWinsForcedDeadline(t *testing.T) {
	socket := newResponseSocket()
	want := socketEvent{body: []byte("ready")}
	socket.events <- want

	got, forced, err := socket.nextBefore(context.Background(), time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("next before: %v", err)
	}
	if forced {
		t.Fatal("forced switch won over an already-buffered socket event")
	}
	if string(got.body) != string(want.body) {
		t.Errorf("body: got %q, want %q", got.body, want.body)
	}
}
