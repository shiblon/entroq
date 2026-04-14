package async_test

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/async"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/eqsvcgrpc"
	"github.com/shiblon/entroq/pkg/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	pb "github.com/shiblon/entroq/api"
)

func startEQ(ctx context.Context) (*entroq.EntroQ, func()) {
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	svc, err := eqsvcgrpc.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatalf("eqsvcgrpc: %v", err)
	}
	srv := grpc.NewServer()
	pb.RegisterEntroQServer(srv, svc)
	grpc_health_v1.RegisterHealthServer(srv, health.NewServer())
	go srv.Serve(lis) //nolint:errcheck

	eq, err := entroq.New(ctx, eqgrpc.Opener(lis.Addr().String(),
		eqgrpc.WithDialOpts(grpc.WithTransportCredentials(insecure.NewCredentials())),
	))
	if err != nil {
		log.Fatalf("entroq client: %v", err)
	}
	return eq, func() {
		eq.Close()
		srv.Stop()
	}
}

// Example_sidecar demonstrates a single-EQ sidecar round-trip: the Sender
// translates an HTTP call into a task, a Receiver worker claims the task and
// forwards it to an upstream service, and the response travels back through
// the queue to the original caller.
func Example_sidecar() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Upstream service: echoes the request path.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	}))
	defer upstream.Close()

	eq, stopEQ := startEQ(ctx)
	defer stopEQ()

	const queue = "echo-svc"

	// Receiver worker: claims Envelope tasks, forwards to upstream, enqueues response.
	rcvCtx, rcvCancel := context.WithCancel(ctx)
	defer rcvCancel()
	recv := worker.New(eq, worker.WithDoModify(async.ReceiverHandler(upstream.URL)))
	go recv.Run(rcvCtx, worker.Watching(queue)) //nolint:errcheck

	// Sender: the first path segment names the target queue; the rest is forwarded.
	sender := async.NewSender(eq, "", queue)
	req := httptest.NewRequest(http.MethodGet,
		"http://local/"+queue+"/hello", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	sender.ServeHTTP(w, req)

	fmt.Printf("status: %d\n", w.Code)
	fmt.Printf("body: %s\n", w.Body)

	// Output:
	// status: 200
	// body: {"path":"/hello"}
}
