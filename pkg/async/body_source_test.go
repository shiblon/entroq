package async

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

type blockingReadCloser struct {
	readStarted chan struct{}
	closed      chan struct{}
	startOnce   sync.Once
	closeOnce   sync.Once
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{
		readStarted: make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

func (b *blockingReadCloser) Read([]byte) (int, error) {
	b.startOnce.Do(func() { close(b.readStarted) })
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *blockingReadCloser) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestBodySourceCancellationClosesBlockedBody(t *testing.T) {
	testCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()

	ctx, cancel := context.WithCancel(testCtx)
	body := newBlockingReadCloser()
	source := newBodySource(body, nil)
	done := make(chan error, 1)
	go func() { done <- source.run(ctx) }()

	select {
	case <-body.readStarted:
	case <-testCtx.Done():
		t.Fatal("body source did not begin reading")
	}
	cancel()

	select {
	case <-body.closed:
	case <-testCtx.Done():
		t.Fatal("body source cancellation did not close the blocked body")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("body source run: %v", err)
		}
	case <-testCtx.Done():
		t.Fatal("body source did not stop after cancellation")
	}
}
