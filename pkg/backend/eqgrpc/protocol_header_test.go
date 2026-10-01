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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestServerSendsProtocolHeaders checks that every response carries the
// protocols the server serves, from which a client chooses, and its release,
// and that the server refuses a request declaring a protocol it does not
// serve.
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
	if got, want := md.Get(version.ProtocolHeader), version.FormatProtocols(version.ServedProtocols); len(got) != 1 || got[0] != want {
		t.Errorf("Protocol header: got %v, want %q", got, want)
	}
	if got := md.Get(version.VersionHeader); len(got) != 1 || got[0] != version.Version {
		t.Errorf("Version header: got %v, want %q", got, version.Version)
	}

	ahead := metadata.AppendToOutgoingContext(ctx, version.ProtocolHeader, strconv.Itoa(int(version.Protocol)+1))
	if _, err := pb.NewEntroQClient(conn).Time(ahead, new(pb.TimeRequest)); status.Code(err) != codes.Unimplemented {
		t.Errorf("Request declaring an unserved protocol: want Unimplemented, got %v", err)
	}
}
