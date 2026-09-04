package mesh

import (
	"context"
	"encoding/json"
	"io/fs"
	"testing"

	openpolicy "github.com/open-policy-agent/opa/v1/rego"
	"github.com/open-policy-agent/opa/v1/storage/inmem"
	"github.com/shiblon/entroq/pkg/authn"
	"github.com/shiblon/entroq/pkg/authz"
	"github.com/shiblon/entroq/pkg/authz/meshpolicy"
	opadata "github.com/shiblon/entroq/pkg/authz/opadata"
)

var kubernetesPolicyFiles = []string{
	"conf/core/entroq/authz/core-entroq-authz.rego",
	"conf/core/entroq/namespaces/core-entroq-namespaces.rego",
	"conf/core/entroq/queues/core-entroq-queues.rego",
	"conf/providers/k8s/permissions/k8s-entroq-permissions.rego",
	"conf/providers/k8s/user/k8s-entroq-user.rego",
}

func TestMatchesKubernetesRegoPolicy(t *testing.T) {
	ctx := context.Background()
	document := gatewayMesh()
	native := New()
	if err := native.ReplaceMesh(ctx, document); err != nil {
		t.Fatalf("ReplaceMesh: %v", err)
	}
	reference := prepareRegoReference(t, document)

	requests := map[string]*authz.Request{
		"exact grant":       insertRequest(gatewaySubject, leafInbox),
		"response grant":    insertRequest(gatewaySubject, "/payments/leaf/response/id"),
		"own prefix":        insertRequest(gatewaySubject, "/payments/gateway/work"),
		"other prefix":      insertRequest(gatewaySubject, "/payments/other/work"),
		"missing principal": insertRequest("", leafInbox),
		"prefix request": {
			Principal: &authn.VerifiedPrincipal{Subject: gatewaySubject},
			Queues:    []*authz.Queue{{Prefix: "/payments/gateway/jobs/", Actions: []authz.Action{authz.Claim, authz.Delete}}},
		},
		"unnamed queue": {
			Principal: &authn.VerifiedPrincipal{Subject: gatewaySubject},
			Queues:    []*authz.Queue{{Actions: []authz.Action{authz.Insert}}},
		},
		"namespace grant": {
			Principal:  &authn.VerifiedPrincipal{Subject: gatewaySubject},
			Namespaces: []*authz.Namespace{{Exact: "/payments/shared/doc", Actions: []authz.Action{authz.Read}}},
		},
		"namespace denied": {
			Principal:  &authn.VerifiedPrincipal{Subject: gatewaySubject},
			Namespaces: []*authz.Namespace{{Exact: "/private/doc", Actions: []authz.Action{authz.Read}}},
		},
		"matching claimant": func() *authz.Request {
			req := insertRequest(gatewaySubject, leafInbox)
			req.ClaimantId = gatewaySubject + "#worker"
			return req
		}(),
		"mismatched claimant": func() *authz.Request {
			req := insertRequest(gatewaySubject, leafInbox)
			req.ClaimantId = "somebody-else#worker"
			return req
		}(),
	}

	for name, req := range requests {
		t.Run(name, func(t *testing.T) {
			nativeAllowed := native.Authorize(ctx, req) == nil
			regoAllowed := evaluateRegoReference(t, reference, req)
			if nativeAllowed != regoAllowed {
				t.Fatalf("native allow = %t, Rego allow = %t", nativeAllowed, regoAllowed)
			}
		})
	}
}

func prepareRegoReference(t *testing.T, document meshpolicy.Document) openpolicy.PreparedEvalQuery {
	t.Helper()
	store := inmem.NewFromObject(map[string]any{"mesh": document})
	options := []func(*openpolicy.Rego){
		openpolicy.Query("data.entroq.authz"),
		openpolicy.Store(store),
	}
	for _, path := range kubernetesPolicyFiles {
		source, err := fs.ReadFile(opadata.ConfFS, path)
		if err != nil {
			t.Fatalf("read Rego policy %q: %v", path, err)
		}
		options = append(options, openpolicy.Module(path, string(source)))
	}
	query, err := openpolicy.New(options...).PrepareForEval(context.Background())
	if err != nil {
		t.Fatalf("prepare Rego policy: %v", err)
	}
	return query
}

func evaluateRegoReference(
	t *testing.T,
	query openpolicy.PreparedEvalQuery,
	req *authz.Request,
) bool {
	t.Helper()
	results, err := query.Eval(context.Background(), openpolicy.EvalInput(req))
	if err != nil {
		t.Fatalf("evaluate Rego policy: %v", err)
	}
	if len(results) != 1 || len(results[0].Expressions) != 1 {
		t.Fatalf("Rego returned no decision: %#v", results)
	}
	data, err := json.Marshal(results[0].Expressions[0].Value)
	if err != nil {
		t.Fatalf("marshal Rego decision: %v", err)
	}
	var decision authz.AuthzError
	if err := json.Unmarshal(data, &decision); err != nil {
		t.Fatalf("unmarshal Rego decision: %v", err)
	}
	return decision.Allow
}
