package mem

import (
	"context"
	"testing"

	"github.com/shiblon/entroq"
	grpcbackend "github.com/shiblon/entroq/grpc"
	"github.com/shiblon/entroq/qsvc/qtest"
)

func TestSimpleSequence(t *testing.T) {
	ctx := context.Background()

	server, dial, err := qtest.StartService(ctx, Opener())
	if err != nil {
		t.Fatalf("Could not start service: %v", err)
	}
	defer server.Stop()

	client, err := entroq.New(ctx, grpcbackend.Opener("bufnet",
		grpcbackend.WithNiladicDialer(dial),
		grpcbackend.WithInsecure()))
	if err != nil {
		t.Fatalf("Open client: %v", err)
	}
	defer client.Close()

	qtest.SimpleSequence(ctx, t, client)
}

func TestSimpleWorker(t *testing.T) {
	ctx := context.Background()

	server, dial, err := qtest.StartService(ctx, Opener())
	if err != nil {
		t.Fatalf("Could not start service: %v", err)
	}
	defer server.Stop()

	client, err := entroq.New(ctx, grpcbackend.Opener("bufnet",
		grpcbackend.WithNiladicDialer(dial),
		grpcbackend.WithInsecure()))
	if err != nil {
		t.Fatalf("Open client: %v", err)
	}
	defer client.Close()

	qtest.SimpleWorker(ctx, t, client)
}
