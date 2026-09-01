package authz

import (
	"context"
	"testing"

	"github.com/open-policy-agent/opa/rego"
	"github.com/open-policy-agent/opa/storage/inmem"
)

func evalAllow(t *testing.T, input map[string]any, storeData map[string]any) bool {
	t.Helper()
	modules, err := parseModules(func(path string) bool {
		return hasPrefix(path, []string{"conf/core/", "conf/providers/entroq/"})
	})
	if err != nil {
		t.Fatalf("parse modules: %v", err)
	}
	options := []func(*rego.Rego){
		rego.Query("data.entroq.authz.allow"),
		rego.Input(input),
		rego.Store(inmem.NewFromObject(storeData)),
	}
	for _, module := range modules {
		options = append(options, rego.ParsedModule(module))
	}
	results, err := rego.New(options...).Eval(context.Background())
	if err != nil {
		t.Fatalf("evaluate policy: %v", err)
	}
	if len(results) == 0 || len(results[0].Expressions) == 0 {
		return false
	}
	allowed, _ := results[0].Expressions[0].Value.(bool)
	return allowed
}

func TestVerifiedPrincipalPolicy(t *testing.T) {
	storeData := map[string]any{
		"entroq": map[string]any{
			"policy": map[string]any{
				"users": []any{map[string]any{
					"name":  "auser",
					"roles": []any{},
					"queues": []any{map[string]any{
						"exact":   "/shared/inbox",
						"actions": []any{"CLAIM", "DELETE"},
					}},
				}},
				"roles": []any{},
			},
		},
	}
	principal := map[string]any{
		"subject":    "auser",
		"issuer":     "https://issuer.example",
		"audience":   []any{"entroq"},
		"expires_at": 2000000000,
		"claims":     map[string]any{"sub": "auser", "role": "worker"},
	}

	for _, tc := range []struct {
		name       string
		principal  any
		claimantID string
		queue      string
		action     string
		want       bool
	}{
		{name: "allowed", principal: principal, queue: "/shared/inbox", action: "CLAIM", want: true},
		{name: "claimant matches", principal: principal, claimantID: "auser#worker-1", queue: "/shared/inbox", action: "DELETE", want: true},
		{name: "claimant mismatch", principal: principal, claimantID: "other#worker-1", queue: "/shared/inbox", action: "CLAIM"},
		{name: "disallowed action", principal: principal, queue: "/shared/inbox", action: "INSERT"},
		{name: "disallowed queue", principal: principal, queue: "/shared/secret", action: "CLAIM"},
		{name: "missing principal", queue: "/shared/inbox", action: "CLAIM"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := map[string]any{
				"claimant_id": tc.claimantID,
				"queues": []any{map[string]any{
					"exact":   tc.queue,
					"actions": []any{tc.action},
				}},
			}
			if tc.principal != nil {
				input["principal"] = tc.principal
			}
			if got := evalAllow(t, input, storeData); got != tc.want {
				t.Fatalf("allow = %v, want %v", got, tc.want)
			}
		})
	}
}
