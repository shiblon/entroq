package cmd

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestOpenEntroqWaitsForStartup(t *testing.T) {
	addr := unusedAddress(t)
	started := make(chan *grpc.Server, 1)
	startErr := make(chan error, 1)
	go func() {
		timer := time.NewTimer(50 * time.Millisecond)
		defer timer.Stop()
		<-timer.C

		listener, err := net.Listen("tcp", addr)
		if err != nil {
			startErr <- err
			return
		}
		server := grpc.NewServer()
		healthpb.RegisterHealthServer(server, health.NewServer())
		started <- server
		if err := server.Serve(listener); err != nil {
			startErr <- err
		}
	}()

	// gRPC's first reconnect after a refused dial uses its normal one-second
	// backoff. Leave enough room to prove that retry rather than tuning it for
	// the test.
	setStartupTimeout(t, 3*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	group, groupCtx := errgroup.WithContext(ctx)

	eq, err := openEntroq(groupCtx, group, "test", addr, "", "", "", "")
	if err != nil {
		select {
		case startErr := <-startErr:
			t.Fatalf("start health server: %v (open error: %v)", startErr, err)
		default:
		}
		t.Fatalf("open EntroQ: %v", err)
	}
	defer eq.Close()

	select {
	case server := <-started:
		server.Stop()
	case err := <-startErr:
		t.Fatalf("start health server: %v", err)
	case <-ctx.Done():
		t.Fatal("health server did not start")
	}
}

func TestOpenEntroqStartupTimeout(t *testing.T) {
	setStartupTimeout(t, 30*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	group, groupCtx := errgroup.WithContext(ctx)

	started := time.Now()
	_, err := openEntroq(groupCtx, group, "test", unusedAddress(t), "", "", "", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("open error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("startup timeout took %v, want no more than 500ms", elapsed)
	}
}

func TestOpenEntroqRejectsNonpositiveStartupTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		setStartupTimeout(t, timeout)
		group, groupCtx := errgroup.WithContext(context.Background())

		_, err := openEntroq(groupCtx, group, "test", "unused", "", "", "", "")
		if err == nil || err.Error() != "--entroq-startup-timeout must be positive" {
			t.Fatalf("timeout %v: open error = %v, want positive-timeout error", timeout, err)
		}
	}
}

func setStartupTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	old := entroqStartupTimeout
	entroqStartupTimeout = timeout
	t.Cleanup(func() { entroqStartupTimeout = old })
}

func unusedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release address: %v", err)
	}
	return addr
}
