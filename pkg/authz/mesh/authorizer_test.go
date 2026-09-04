package mesh

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/shiblon/entroq/pkg/authn"
	"github.com/shiblon/entroq/pkg/authz"
	"github.com/shiblon/entroq/pkg/authz/meshpolicy"
)

const (
	gatewaySubject = "system:serviceaccount:payments:gateway"
	leafInbox      = "/payments/leaf/inbox"
)

func TestReplaceMeshChangesSubsequentDecisions(t *testing.T) {
	ctx := context.Background()
	a := New()
	if a.Ready() {
		t.Fatal("new authorizer is ready before receiving mesh policy")
	}

	req := insertRequest(gatewaySubject, leafInbox)
	assertDenied(t, a.Authorize(ctx, req))

	if err := a.ReplaceMesh(ctx, gatewayMesh()); err != nil {
		t.Fatalf("ReplaceMesh: %v", err)
	}
	if !a.Ready() {
		t.Fatal("authorizer is not ready after receiving mesh policy")
	}
	if err := a.Authorize(ctx, req); err != nil {
		t.Fatalf("Authorize after ReplaceMesh: %v", err)
	}
}

func TestKubernetesPolicyCorpus(t *testing.T) {
	ctx := context.Background()
	a := New()
	if err := a.ReplaceMesh(ctx, gatewayMesh()); err != nil {
		t.Fatalf("ReplaceMesh: %v", err)
	}

	for _, tc := range []struct {
		name string
		req  *authz.Request
		want bool
	}{
		{name: "exact mesh grant", req: insertRequest(gatewaySubject, leafInbox), want: true},
		{name: "response prefix grant", req: insertRequest(gatewaySubject, "/payments/leaf/response/request-id"), want: true},
		{name: "label mismatch", req: insertRequest("system:serviceaccount:payments:worker", leafInbox)},
		{name: "own prefix", req: insertRequest(gatewaySubject, "/payments/gateway/work"), want: true},
		{name: "other prefix", req: insertRequest(gatewaySubject, "/payments/other/work")},
		{
			name: "namespace grant",
			req: &authz.Request{
				Principal:  &authn.VerifiedPrincipal{Subject: gatewaySubject},
				Namespaces: []*authz.Namespace{{Prefix: "/payments/shared/docs", Actions: []authz.Action{authz.Read}}},
			},
			want: true,
		},
		{
			name: "claimant matches",
			req: func() *authz.Request {
				req := insertRequest(gatewaySubject, leafInbox)
				req.ClaimantId = gatewaySubject + "#worker-1"
				return req
			}(),
			want: true,
		},
		{
			name: "claimant mismatch",
			req: func() *authz.Request {
				req := insertRequest(gatewaySubject, leafInbox)
				req.ClaimantId = "system:serviceaccount:payments:other#worker-1"
				return req
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := a.Authorize(ctx, tc.req)
			if tc.want && err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if !tc.want {
				assertDenied(t, err)
			}
		})
	}
}

func TestCallerLabelsUseANDWithinMatcherAndORAcrossMatchers(t *testing.T) {
	document := gatewayMesh()
	document.Identities[gatewaySubject] = meshpolicy.Identity{Labels: map[string]string{
		"app":         "gateway",
		"environment": "production",
	}}
	document.Queues[0].AllowedCallers = []map[string]string{
		{"app": "gateway", "environment": "staging"},
		{"app": "gateway", "environment": "production"},
	}

	a := New()
	if err := a.ReplaceMesh(context.Background(), document); err != nil {
		t.Fatalf("ReplaceMesh: %v", err)
	}
	if err := a.Authorize(context.Background(), insertRequest(gatewaySubject, leafInbox)); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
}

func TestRejectedMeshPreservesLastGoodPolicy(t *testing.T) {
	ctx := context.Background()
	a := New()
	if err := a.ReplaceMesh(ctx, gatewayMesh()); err != nil {
		t.Fatalf("ReplaceMesh(valid): %v", err)
	}

	bad := gatewayMesh()
	bad.Queues[0].MatchType = "Regex"
	if err := a.ReplaceMesh(ctx, bad); err == nil {
		t.Fatal("ReplaceMesh(invalid) succeeded")
	}
	if err := a.Authorize(ctx, insertRequest(gatewaySubject, leafInbox)); err != nil {
		t.Fatalf("last good policy was not retained: %v", err)
	}
}

func TestConcurrentAuthorizeAndReplaceMesh(t *testing.T) {
	ctx := context.Background()
	a := New()
	if err := a.ReplaceMesh(ctx, gatewayMesh()); err != nil {
		t.Fatalf("ReplaceMesh: %v", err)
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_ = a.Authorize(ctx, insertRequest(gatewaySubject, leafInbox))
			}
		}()
	}
	for range 20 {
		if err := a.ReplaceMesh(ctx, gatewayMesh()); err != nil {
			t.Fatalf("ReplaceMesh: %v", err)
		}
	}
	wg.Wait()
}

func TestOwnPrefixBeforeMeshInitialization(t *testing.T) {
	a := New()
	if err := a.Authorize(
		context.Background(),
		insertRequest(gatewaySubject, "/payments/gateway/inbox"),
	); err != nil {
		t.Fatalf("Authorize own prefix: %v", err)
	}
}

func gatewayMesh() meshpolicy.Document {
	return meshpolicy.Document{
		Initialized: true,
		Identities: map[string]meshpolicy.Identity{
			gatewaySubject: {Labels: map[string]string{"app": "gateway"}},
		},
		Queues: []meshpolicy.QueuePolicy{{
			Pattern:        leafInbox,
			MatchType:      "Exact",
			AllowedCallers: []map[string]string{{"app": "gateway"}},
		}},
		Namespaces: []meshpolicy.NamespacePolicy{{
			Pattern:        "/payments/shared/",
			MatchType:      "Prefix",
			AllowedCallers: []map[string]string{{"app": "gateway"}},
		}},
	}
}

func insertRequest(subject, queue string) *authz.Request {
	return &authz.Request{
		Principal: &authn.VerifiedPrincipal{Subject: subject},
		Queues: []*authz.Queue{{
			Exact:   queue,
			Actions: []authz.Action{authz.Insert},
		}},
	}
}

func assertDenied(t *testing.T, err error) {
	t.Helper()
	var authzErr *authz.AuthzError
	if !errors.As(err, &authzErr) || authzErr.Allow {
		t.Fatalf("Authorize error = %v, want denial", err)
	}
}
