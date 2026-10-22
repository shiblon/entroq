package eqgrpc_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	hpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// protocolServer is an EntroQ server that lists the protocols it was given,
// or none, as a server from before the protocol header, and records the
// protocols its requests declare.
type protocolServer struct {
	pb.UnimplementedEntroQServer
	served string // "" sends no header

	mu       sync.Mutex
	declared []string // per request: the protocol it declared
	claims   int
}

func (s *protocolServer) note(ctx context.Context) {
	if s.served != "" {
		_ = grpc.SetHeader(ctx, metadata.Pairs(version.ProtocolHeader, s.served))
	}
	md, _ := metadata.FromIncomingContext(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.declared = append(s.declared, strings.Join(md.Get(version.ProtocolHeader), ","))
}

func (s *protocolServer) Time(ctx context.Context, _ *pb.TimeRequest) (*pb.TimeResponse, error) {
	s.note(ctx)
	return &pb.TimeResponse{TimeMs: time.Now().UnixMilli()}, nil
}

func (s *protocolServer) TryClaim(ctx context.Context, _ *pb.ClaimRequest) (*pb.ClaimResponse, error) {
	s.note(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	return new(pb.ClaimResponse), nil
}

func serveProtocols(t *testing.T, ctx context.Context, served string) (*protocolServer, *entroq.EntroQ) {
	t.Helper()
	fake := &protocolServer{served: served}
	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer()
	hpb.RegisterHealthServer(s, health.NewServer())
	pb.RegisterEntroQServer(s, fake)
	go s.Serve(lis)
	t.Cleanup(s.Stop)
	client, err := entroq.New(ctx, eqgrpc.Opener("bufnet", eqgrpc.WithNiladicDialer(lis.Dial), eqgrpc.WithInsecure()))
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return fake, client
}

// TestNegotiation checks that the client works only with a server that
// serves its protocol, refusing any other before making the call, and that
// it declares its protocol on every request.
func TestNegotiation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, tc := range []struct {
		name, served, upgrade string
	}{
		{"a server from before the header", "", "the server"},
		{"a server behind", "1", "the server"},
		{"a server ahead", "3,4", "this client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, client := serveProtocols(t, ctx, tc.served)
			_, err := client.TryClaim(ctx, entroq.From("q"))
			if !entroq.IsUnsupported(err) || !strings.Contains(err.Error(), "upgrade "+tc.upgrade) {
				t.Errorf("Claim: want an unsupported error saying to upgrade %s, got %v", tc.upgrade, err)
			}
			if fake.claims != 0 {
				t.Errorf("Claim: the server received it; want it refused before sending")
			}
		})
	}

	t.Run("a server serving the client's protocol", func(t *testing.T) {
		fake, client := serveProtocols(t, ctx, "1,2,3")
		if _, err := client.TryClaim(ctx, entroq.From("q")); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.claims != 1 {
			t.Errorf("Claim: want it sent once, got %d", fake.claims)
		}
		for i, d := range fake.declared {
			if d != "2" {
				t.Errorf("Request %d declared protocol %q, want %q", i, d, "2")
			}
		}
	})
}
