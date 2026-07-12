package eqgrpc_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/eqsvcgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/shiblon/entroq/api"
	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

// TestGRPCClaimNotLostWhenDeliverySlow is a deterministic regression test for
// the lost-claim race: a claim the server has committed must reach the caller,
// even if its response is delivered more slowly than the client's retry
// interval.
//
// The bug: the client used to bound each Claim attempt with a hard context
// deadline equal to claimRetryInterval. Under load a claim could commit
// server-side (task marked claimed for its whole duration) while the response
// was still in flight; if the client's deadline fired first, gRPC returned
// DeadlineExceeded and the client discarded the delivered task. That task then
// sat claimed-but-unowned until the claim expired (DefaultClaimDuration), so a
// rapidly re-issuing client would appear to hang for ~30s.
//
// We force the race deterministically with a server interceptor that delays a
// successful claim's response well past the retry interval. With the fix (the
// client lets the server bound each attempt via PollMs and never cancels an
// in-flight claim) the task is delivered and claimed. Before the fix this hangs
// until the context deadline.
func TestGRPCClaimNotLostWhenDeliverySlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const (
		retry     = 50 * time.Millisecond  // client retry interval
		respDelay = 300 * time.Millisecond // delivery delay, deliberately >> retry
	)

	svc, err := eqsvcgrpc.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	// Interceptor that delays only a successful Claim's response, simulating slow
	// delivery of an already-committed claim. TryClaim and other RPCs are
	// untouched (their method does not end in "/Claim").
	delayClaim := func(ictx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, herr := handler(ictx, req)
		if herr == nil && strings.HasSuffix(info.FullMethod, "/Claim") {
			if cr, ok := resp.(*pb.ClaimResponse); ok && cr.GetTask() != nil {
				select {
				case <-time.After(respDelay):
				case <-ictx.Done():
				}
			}
		}
		return resp, herr
	}

	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer(grpc.UnaryInterceptor(delayClaim))
	hpb.RegisterHealthServer(s, health.NewServer())
	pb.RegisterEntroQServer(s, svc)
	go s.Serve(lis)
	defer s.Stop()

	client, err := entroq.New(ctx, eqgrpc.Opener("bufnet",
		eqgrpc.WithNiladicDialer(lis.Dial),
		eqgrpc.WithInsecure(),
		eqgrpc.WithClaimRetryInterval(retry),
	))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	const q = "test/slow-delivery"

	// Insert a task so the first claim attempt commits immediately; the
	// interceptor then stalls its delivery past the retry interval.
	if _, err := client.Modify(ctx, entroq.InsertingInto(q, entroq.WithValue("hi"))); err != nil {
		t.Fatalf("insert: %v", err)
	}

	claimed := make(chan *entroq.Task, 1)
	errc := make(chan error, 1)
	go func() {
		task, err := client.Claim(ctx, entroq.From(q))
		if err != nil {
			errc <- err
			return
		}
		claimed <- task
	}()

	select {
	case task := <-claimed:
		if task == nil {
			t.Fatal("claim returned a nil task")
		}
		if task.Queue != q {
			t.Fatalf("claimed task from queue %q, want %q", task.Queue, q)
		}
	case err := <-errc:
		t.Fatalf("claim returned an error: %v", err)
	case <-ctx.Done():
		t.Fatal("claim never returned: a slowly-delivered claim was lost and the task stranded")
	}
}
