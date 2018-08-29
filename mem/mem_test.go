package mem

import (
	"context"
	"testing"

	pb "github.com/shiblon/entroq/qsvc/proto"
	"github.com/shiblon/entroq/qsvc/test"
)

func TestSimpleSequence(t *testing.T) {
	ctx := context.Background()

	server, dial, err := test.StartService(ctx, Opener())
	if err != nil {
		t.Fatalf("Could not start service: %v", err)
	}
	defer server.Stop()

	conn, err := dial(ctx)
	if err != nil {
		t.Fatalf("Could not dial: %v", err)
	}
	defer conn.Close()

	test.SimpleSequence(ctx, t, pb.NewEntroQClient(conn))
}
