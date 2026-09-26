package eqgrpc

import "testing"

func TestServerKeepalivePolicyAcceptsDefaultClient(t *testing.T) {
	policy := serverKeepalivePolicy()
	if policy.MinTime > DefaultKeepaliveTime {
		t.Fatalf("server minimum ping interval %v exceeds client interval %v",
			policy.MinTime, DefaultKeepaliveTime)
	}
	if policy.PermitWithoutStream {
		t.Fatal("server permits keepalive without an active RPC")
	}
}
