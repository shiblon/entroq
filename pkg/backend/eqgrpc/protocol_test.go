package eqgrpc

import (
	"context"
	"strings"
	"testing"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/version"
	"google.golang.org/grpc/metadata"
)

func TestProtocolOf(t *testing.T) {
	for _, tc := range []struct {
		md   metadata.MD
		want int32
	}{
		{metadata.Pairs("content-type", "application/grpc"), 1},
		{metadata.Pairs(version.ProtocolHeader, "2"), 2},
		{metadata.Pairs(version.ProtocolHeader, "7"), 7},
		{metadata.Pairs(version.ProtocolHeader, "nonsense"), 1},
	} {
		if got := protocolOf(tc.md); got != tc.want {
			t.Errorf("protocolOf(%v) = %d, want %d", tc.md, got, tc.want)
		}
	}
}

// TestArrivalsNeedProtocol2 checks that arrival changes are never sent to a
// server below protocol 2, which would apply them as changes with empty data.
func TestArrivalsNeedProtocol2(t *testing.T) {
	b := new(backend) // no connection: nothing may be sent
	b.protocol.Store(1)
	mod := entroq.NewModification("me", entroq.Arriving(entroq.ReadyNow().Tasks(&entroq.Task{ID: "t", Queue: "q"})))
	if _, err := b.Modify(context.Background(), mod); err == nil || !strings.Contains(err.Error(), "protocol 2") {
		t.Errorf("Arrival to a protocol 1 server: want a refusal naming protocol 2, got %v", err)
	}
}
