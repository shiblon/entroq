package authz

import (
	"encoding/json"
	"testing"
)

func TestNewYAMLRequestWithVerifiedPrincipal(t *testing.T) {
	req, err := NewYAMLRequest(`
principal:
  subject: service-a
  issuer: https://issuer.example
  audience: [entroq]
  expires_at: 2000000000
  claims:
    sub: service-a
    role: gateway
claimant_id: service-a#worker-1
queues:
  - exact: /service-b/inbox
    actions: [INSERT]
`)
	if err != nil {
		t.Fatalf("NewYAMLRequest: %v", err)
	}
	if req.Principal == nil || req.Principal.Subject != "service-a" {
		t.Fatalf("principal = %#v, want service-a", req.Principal)
	}
	if req.ClaimantId != "service-a#worker-1" {
		t.Fatalf("claimant ID = %q", req.ClaimantId)
	}
	var claims map[string]any
	if err := json.Unmarshal(req.Principal.Claims, &claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	if claims["role"] != "gateway" {
		t.Fatalf("claims = %#v, want gateway role", claims)
	}
}
