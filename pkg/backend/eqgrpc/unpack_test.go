package eqgrpc

import (
	"testing"

	"github.com/shiblon/entroq"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestUnpackUnavailable checks that a gRPC Unavailable status is translated to
// entroq's transient-unavailable error. This is the seam that lets the work
// gateway ride out a restarting or relocating backend: it classifies transient
// EntroQ outages via entroq.IsUnavailable rather than inspecting gRPC codes.
func TestUnpackUnavailable(t *testing.T) {
	err := unpackGRPCError(status.Error(codes.Unavailable, "connection refused"))
	if !entroq.IsUnavailable(err) {
		t.Fatalf("unpackGRPCError(Unavailable) = %v, want an entroq.IsUnavailable error", err)
	}
}
