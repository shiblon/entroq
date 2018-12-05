package mem

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	grpcbackend "github.com/shiblon/entroq/grpc"
	"github.com/shiblon/entroq/qsvc/qtest"
	"google.golang.org/grpc"
)

func TestSimpleSequence(t *testing.T) {
	ctx := context.Background()

	server, dial, err := qtest.StartService(ctx, Opener())
	if err != nil {
		t.Fatalf("Could not start service: %v", err)
	}
	defer server.Stop()

	client, err := entroq.New(ctx, grpcbackend.Opener("bufnet",
		grpc.WithDialer(func(string, time.Duration) (net.Conn, error) {
			return dial()
		}),
		grpc.WithInsecure()))
	if err != nil {
		t.Fatalf("Open client: %v", err)
	}
	defer client.Close()

	qtest.SimpleSequence(ctx, t, client)
}
