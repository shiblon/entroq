package eqgrpc_test

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
	"github.com/shiblon/entroq/pkg/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// TestServerSendsProtocolHeaders checks that every response carries the
// server's protocol and release, which a client reads before sending anything
// newer than protocol 1.
func TestServerSendsProtocolHeaders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stop, dial, err := eqtest.StartService(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("Start service: %v", err)
	}
	defer stop()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	var md metadata.MD
	if _, err := pb.NewEntroQClient(conn).Time(ctx, new(pb.TimeRequest), grpc.Header(&md)); err != nil {
		t.Fatalf("Time: %v", err)
	}
	if got := md.Get(version.ProtocolHeader); len(got) != 1 || got[0] != strconv.Itoa(version.Protocol) {
		t.Errorf("Protocol header: got %v, want %d", got, version.Protocol)
	}
	if got := md.Get(version.VersionHeader); len(got) != 1 || got[0] != version.Version {
		t.Errorf("Version header: got %v, want %q", got, version.Version)
	}
}
