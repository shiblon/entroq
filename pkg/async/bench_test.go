package async_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/backend/eqgrpc"
	"github.com/shiblon/entroq/backend/eqmem"
	"github.com/shiblon/entroq/pkg/async"
	"github.com/shiblon/entroq/pkg/eqsvcgrpc"
	"github.com/shiblon/entroq/pkg/worker"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	pb "github.com/shiblon/entroq/api"
)

// mustStartEntroQ starts a real gRPC server on a loopback TCP port backed by
// the given opener. Returns a connected EntroQ client and a stop function.
// Calls t.Fatal on any setup error.
func mustStartEntroQ(ctx context.Context, t testing.TB, opener entroq.BackendOpener) (*entroq.EntroQ, func()) {
	t.Helper()

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	svc, err := eqsvcgrpc.New(ctx, opener)
	if err != nil {
		lis.Close()
		t.Fatalf("eqsvcgrpc: %v", err)
	}

	srv := grpc.NewServer()
	pb.RegisterEntroQServer(srv, svc)
	grpc_health_v1.RegisterHealthServer(srv, health.NewServer())
	go srv.Serve(lis)

	addr := lis.Addr().String()
	eq, err := entroq.New(ctx, eqgrpc.Opener(addr,
		eqgrpc.WithDialOpts(grpc.WithTransportCredentials(insecure.NewCredentials())),
	))
	if err != nil {
		srv.Stop()
		t.Fatalf("entroq client: %v", err)
	}

	return eq, func() {
		eq.Close()
		srv.Stop()
	}
}

// upstreamHandler is the trivial upstream used in all benchmarks: always
// returns 200 OK with a small fixed body.
var upstreamHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"ok":true}`)
})

// startUpstream starts upstreamHandler as a real HTTP service on a loopback
// port, for use as the receiver's upstream.
func startUpstream() *httptest.Server {
	return httptest.NewServer(upstreamHandler)
}

// mustStartReceivers starts concurrency receiver goroutines claiming from
// queue and forwarding to upstream. Returns a stop function.
// Calls t.Fatal on any setup error.
func mustStartReceivers(ctx context.Context, t testing.TB, eq *entroq.EntroQ, queue, upstream string, concurrency int) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(ctx)
	recvWorker := worker.New(eq, worker.WithDoModify(async.ReceiverHandler(upstream)))

	g, gctx := errgroup.WithContext(ctx)
	for range concurrency {
		g.Go(func() error { return recvWorker.Run(gctx, queue) })
	}

	return func() {
		cancel()
		g.Wait()
	}
}

func BenchmarkSidecar(b *testing.B) {
	concurrencies := []int{1, 10, 100}

	for _, n := range concurrencies {
		b.Run(fmt.Sprintf("concurrency=%d", n), func(b *testing.B) {
			ctx := context.Background()

			upstream := startUpstream()
			defer upstream.Close()

			eq, stopEQ := mustStartEntroQ(ctx, b, eqmem.Opener())
			defer stopEQ()

			// Queue name has no leading slash: parsePath extracts the first
			// path segment as the target queue, so it must match the queue
			// receivers claim from.
			queue := fmt.Sprintf("bench-c%d", n)
			stopReceivers := mustStartReceivers(ctx, b, eq, queue, upstream.URL, n)
			defer stopReceivers()

			// Drive the sender via ServeHTTP directly, bypassing TCP between
			// the benchmark client and the sender. This isolates queue
			// round-trip latency from TCP connection overhead.
			sender := async.NewSender(eq, "", queue)
			reqURL := fmt.Sprintf("http://bench/%s/ping", queue)

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					req := httptest.NewRequest(http.MethodGet, reqURL, nil).WithContext(ctx)
					w := httptest.NewRecorder()
					sender.ServeHTTP(w, req)
					if w.Code != http.StatusOK {
						b.Errorf("unexpected status: %d", w.Code)
					}
				}
			})
		})
	}
}

// BenchmarkDirect measures the cost of calling the upstream handler directly,
// with no queue or gRPC involved. The delta between this and BenchmarkSidecar
// is the overhead the sidecar adds per request.
func BenchmarkDirect(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest(http.MethodGet, "/ping", nil)
			w := httptest.NewRecorder()
			upstreamHandler.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				b.Errorf("unexpected status: %d", w.Code)
			}
		}
	})
}
