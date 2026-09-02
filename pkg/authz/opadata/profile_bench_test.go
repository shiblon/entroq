package authz

import (
	"context"
	"fmt"
	"testing"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/rego"
	"github.com/open-policy-agent/opa/storage/inmem"
)

const profileUsername = "system:serviceaccount:mesh-bench:gateway"

func profileInput() map[string]any {
	return map[string]any{
		"principal": map[string]any{
			"subject": profileUsername,
			"claims":  map[string]any{"sub": profileUsername},
		},
		"queues": []any{
			map[string]any{
				"exact":   "/mesh-bench/leaf/inbox",
				"actions": []any{"INSERT"},
			},
		},
	}
}

func benchmarkPreparedPolicy(b *testing.B, modules map[string]*ast.Module, storeData map[string]any) {
	b.Helper()
	options := []func(*rego.Rego){
		rego.Query("data.entroq.authz"),
		rego.Store(inmem.NewFromObject(storeData)),
	}
	for _, module := range modules {
		options = append(options, rego.ParsedModule(module))
	}
	prepared, err := rego.New(options...).PrepareForEval(context.Background())
	if err != nil {
		b.Fatalf("prepare: %v", err)
	}

	input := profileInput()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := prepared.Eval(context.Background(), rego.EvalInput(input)); err != nil {
			b.Fatalf("eval: %v", err)
		}
	}
}

// BenchmarkK8sPolicyScale measures the list-shaped operator document after
// authentication. Exactly one policy matches the verified principal; the rest
// force the current Rego rules to scan unrelated queue policies.
func BenchmarkK8sPolicyScale(b *testing.B) {
	modules, err := parseModules(func(path string) bool {
		return hasPrefix(path, []string{"conf/core/", "conf/providers/k8s/"})
	})
	if err != nil {
		b.Fatalf("parse modules: %v", err)
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
			benchmarkPreparedPolicy(b, modules, map[string]any{
				"mesh": map[string]any{
					"initialized": true,
					"identities": map[string]any{
						profileUsername: map[string]any{"labels": map[string]any{"role": "gateway"}},
					},
					"queues":     policies,
					"namespaces": []any{},
				},
			})
		})
	}
}

// BenchmarkK8sPrecomputedPolicyScale prototypes an operator-produced grants
// index keyed by verified principal. The original policy list remains in the
// data document to prove that its size is irrelevant to indexed decisions.
func BenchmarkK8sPrecomputedPolicyScale(b *testing.B) {
	modules, err := parseModules(func(path string) bool {
		return hasPrefix(path, []string{"conf/core/"}) ||
			path == "conf/providers/k8s/user/k8s-entroq-user.rego"
	})
	if err != nil {
		b.Fatalf("parse modules: %v", err)
	}
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
			benchmarkPreparedPolicy(b, modules, map[string]any{
				"mesh": map[string]any{
					"initialized": true,
					"queues":      policies,
					"grants": map[string]any{
						profileUsername: map[string]any{
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
		})
	}
}
