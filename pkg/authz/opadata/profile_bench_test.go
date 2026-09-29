package authz

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/rego"
	"github.com/open-policy-agent/opa/storage/inmem"
	"github.com/open-policy-agent/opa/topdown/cache"
)

// BenchmarkK8sPolicy separates steady-state JWT verification from the complete
// Kubernetes mesh decision. Both prepared queries share the same policy shape
// as the OPA sidecar and warm the inter-query JWKS cache before timing.
func BenchmarkK8sPolicy(b *testing.B) {
	const (
		audience = "mesh-profile-audience"
		issuer   = "mesh-profile-issuer"
		username = "system:serviceaccount:mesh-bench:gateway"
	)

	srv := jwksServer(b, rsaKey)
	token := makeToken(b, rsaKey, username, audience, issuer, time.Hour)
	modules, err := parseModules(func(path string) bool {
		return hasPrefix(path, []string{"conf/core/", "conf/providers/k8s/"})
	})
	if err != nil {
		b.Fatalf("parse modules: %v", err)
	}

	store := inmem.NewFromObject(map[string]any{
		"entroq": map[string]any{
			"k8s": map[string]any{
				"jwks_url": srv.URL,
				"audience": audience,
				"issuer":   issuer,
			},
		},
		"mesh": map[string]any{
			"initialized": true,
			"identities": map[string]any{
				username: map[string]any{"labels": map[string]any{"role": "gateway"}},
			},
			"queues": []any{
				map[string]any{
					"pattern":        "/mesh-bench/leaf/inbox",
					"matchType":      "Exact",
					"allowedCallers": []any{map[string]any{"role": "gateway"}},
				},
			},
			"namespaces": []any{},
		},
	})
	input := map[string]any{
		"authz": map[string]any{
			"type":        "Bearer",
			"credentials": token,
		},
		"queues": []any{
			map[string]any{
				"exact":   "/mesh-bench/leaf/inbox",
				"actions": []any{"INSERT"},
			},
		},
	}

	for _, benchmark := range []struct {
		name  string
		query string
	}{
		{name: "jwt-only", query: "data.entroq.user.name"},
		{name: "full-decision", query: "data.entroq.authz"},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			config, err := cache.ParseCachingConfig(nil)
			if err != nil {
				b.Fatalf("parse cache config: %v", err)
			}
			interQueryCache := cache.NewInterQueryCache(config)
			options := []func(*rego.Rego){
				rego.Query(benchmark.query),
				rego.Store(store),
				rego.InterQueryBuiltinCache(interQueryCache),
			}
			for _, module := range modules {
				options = append(options, rego.ParsedModule(module))
			}
			prepared, err := rego.New(options...).PrepareForEval(context.Background())
			if err != nil {
				b.Fatalf("prepare %s: %v", benchmark.query, err)
			}

			// Populate the http.send inter-query cache so the benchmark measures
			// repeated verification rather than the first JWKS fetch.
			if _, err := prepared.Eval(context.Background(), rego.EvalInput(input)); err != nil {
				b.Fatalf("warm %s: %v", benchmark.query, err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := prepared.Eval(context.Background(), rego.EvalInput(input)); err != nil {
					b.Fatalf("eval %s: %v", benchmark.query, err)
				}
			}
		})
	}
}

// BenchmarkK8sPolicyScale measures the list-shaped operator document without
// JWT work. Exactly one policy matches the caller; the rest force the current
// Rego rules to scan unrelated queue policies.
func BenchmarkK8sPolicyScale(b *testing.B) {
	const username = "system:serviceaccount:mesh-bench:gateway"
	modules, err := parseModules(func(path string) bool {
		return hasPrefix(path, []string{"conf/core/"}) ||
			path == "conf/providers/k8s/permissions/k8s-entroq-permissions.rego"
	})
	if err != nil {
		b.Fatalf("parse modules: %v", err)
	}
	modules["profile-static-user.rego"] = ast.MustParseModule(`
package entroq.user
import rego.v1
name := input.profile_user
`)
	input := map[string]any{
		"profile_user": username,
		"authz":        map[string]any{},
		"queues": []any{
			map[string]any{
				"exact":   "/mesh-bench/leaf/inbox",
				"actions": []any{"INSERT"},
			},
		},
	}

	for _, policyCount := range []int{2, 10, 100, 1000} {
		b.Run(fmt.Sprintf("queues-%04d", policyCount), func(b *testing.B) {
			policies := make([]any, 0, policyCount)
			policies = append(policies, map[string]any{
				"pattern":        "/mesh-bench/leaf/inbox",
				"matchType":      "Exact",
				"allowedCallers": []any{map[string]any{"role": "gateway"}},
			})
			for i := 1; i < policyCount; i++ {
				policies = append(policies, map[string]any{
					"pattern":        fmt.Sprintf("/unrelated/service-%04d/inbox", i),
					"matchType":      "Exact",
					"allowedCallers": []any{map[string]any{"role": "other"}},
				})
			}
			store := inmem.NewFromObject(map[string]any{
				"mesh": map[string]any{
					"initialized": true,
					"identities": map[string]any{
						username: map[string]any{"labels": map[string]any{"role": "gateway"}},
					},
					"queues":     policies,
					"namespaces": []any{},
				},
			})
			options := []func(*rego.Rego){
				rego.Query("data.entroq.authz"),
				rego.Store(store),
			}
			for _, module := range modules {
				options = append(options, rego.ParsedModule(module))
			}
			prepared, err := rego.New(options...).PrepareForEval(context.Background())
			if err != nil {
				b.Fatalf("prepare: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := prepared.Eval(context.Background(), rego.EvalInput(input)); err != nil {
					b.Fatalf("eval: %v", err)
				}
			}
		})
	}
}

// BenchmarkK8sPrecomputedPolicyScale prototypes an operator-produced grants
// index keyed by authenticated identity. The original policy list remains in
// the data document to ensure its size is irrelevant when decisions use the
// precomputed index.
func BenchmarkK8sPrecomputedPolicyScale(b *testing.B) {
	const username = "system:serviceaccount:mesh-bench:gateway"
	modules, err := parseModules(func(path string) bool {
		return hasPrefix(path, []string{"conf/core/"})
	})
	if err != nil {
		b.Fatalf("parse modules: %v", err)
	}
	modules["profile-static-user.rego"] = ast.MustParseModule(`
package entroq.user
import rego.v1
name := input.profile_user
`)
	modules["profile-precomputed-permissions.rego"] = ast.MustParseModule(`
package entroq.permissions
import rego.v1
import data.entroq.user as equser

allowed_queues contains q if {
	some q in data.mesh.grants[equser.name].queues
}

allowed_namespaces contains n if {
	some n in data.mesh.grants[equser.name].namespaces
}

is_admin := false
`)
	input := map[string]any{
		"profile_user": username,
		"authz":        map[string]any{},
		"queues": []any{
			map[string]any{
				"exact":   "/mesh-bench/leaf/inbox",
				"actions": []any{"INSERT"},
			},
		},
	}

	for _, policyCount := range []int{2, 10, 100, 1000} {
		b.Run(fmt.Sprintf("queues-%04d", policyCount), func(b *testing.B) {
			policies := make([]any, policyCount)
			for i := range policyCount {
				policies[i] = map[string]any{
					"pattern":        fmt.Sprintf("/service-%04d/inbox", i),
					"matchType":      "Exact",
					"allowedCallers": []any{map[string]any{"role": "other"}},
				}
			}
			store := inmem.NewFromObject(map[string]any{
				"mesh": map[string]any{
					"initialized": true,
					"queues":      policies,
					"grants": map[string]any{
						username: map[string]any{
							"queues": []any{
								map[string]any{"prefix": "/mesh-bench/gateway/", "actions": []any{"ALL"}},
								map[string]any{"exact": "/mesh-bench/leaf/inbox", "actions": []any{"ALL"}},
								map[string]any{"prefix": "/mesh-bench/leaf/response/", "actions": []any{"ALL"}},
							},
							"namespaces": []any{},
						},
					},
				},
			})
			options := []func(*rego.Rego){
				rego.Query("data.entroq.authz"),
				rego.Store(store),
			}
			for _, module := range modules {
				options = append(options, rego.ParsedModule(module))
			}
			prepared, err := rego.New(options...).PrepareForEval(context.Background())
			if err != nil {
				b.Fatalf("prepare: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := prepared.Eval(context.Background(), rego.EvalInput(input)); err != nil {
					b.Fatalf("eval: %v", err)
				}
			}
		})
	}
}
