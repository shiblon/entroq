package cmd

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// TestOnSignal_Drain: the first signal drains, and the run's context ends when
// the drain returns.
func TestOnSignal_Drain(t *testing.T) {
	called := make(chan struct{})
	ctx, cancel := onSignal(context.Background(), time.Minute, func(context.Context) error {
		close(called)
		return nil
	})
	defer cancel()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown not called on the first signal")
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("run context did not end after the drain")
	}
}

// TestOnSignal_SecondStops: a second signal ends the run's context while the
// drain is still going, and the drain sees its own context end.
func TestOnSignal_SecondStops(t *testing.T) {
	draining := make(chan struct{})
	drainErr := make(chan error, 1)
	ctx, cancel := onSignal(context.Background(), time.Minute, func(sctx context.Context) error {
		close(draining)
		<-sctx.Done()
		drainErr <- sctx.Err()
		return sctx.Err()
	})
	defer cancel()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("signal: %v", err)
	}
	<-draining
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("run context did not end on the second signal")
	}
	if err := <-drainErr; err == nil {
		t.Fatal("drain context did not end on the second signal")
	}
}

// TestOnSignal_NoSignal: a run that ends on its own never drains.
func TestOnSignal_NoSignal(t *testing.T) {
	ctx, cancel := onSignal(context.Background(), time.Minute, func(context.Context) error {
		t.Error("shutdown called without a signal")
		return nil
	})
	cancel()
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond)
}
