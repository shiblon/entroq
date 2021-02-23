package authzopa

import (
	"context"
	"fmt"
	"os"
	"testing"

	_ "embed"

	"entrogo.com/entroq/pkg/authz"
	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/tester"
)

var (
	//go:embed core-rules_test.rego
	coreRegoTest string

	//go:embed extra-rules.rego
	extraRegoTest string

	//go:embed queues_test.rego
	queuesRegoTest string
)

func parseModules(modules map[string]string) (map[string]*ast.Module, error) {
	result := make(map[string]*ast.Module)

	for k, v := range modules {
		m, err := ast.ParseModule(k, v)
		if err != nil {
			return nil, fmt.Errorf("parse module: %w", err)
		}
		result[k] = m
	}
	return result, nil
}

// TestCoreRules runs the OPA tests found in the file in this directory.
// These are the right way to test OPA in the raw.
func TestCoreRules(t *testing.T) {
	ctx := context.Background()

	mods, err := parseModules(map[string]string{
		"core-rules.rego":      coreRego,
		"extra-rules.rego":     extraRegoTest,
		"queues.rego":          queuesRego,
		"core-rules_test.rego": coreRegoTest,
		"queues_test.rego":     queuesRegoTest,
	})
	if err != nil {
		t.Fatalf("Failed to parse test modules: %v", err)
	}

	rch, err := tester.NewRunner().
		SetModules(mods).
		EnableFailureLine(true).
		EnableTracing(true).
		RunTests(ctx, nil)
	if err != nil {
		t.Fatalf("Error running tests: %v", err)
	}

	if err := (tester.PrettyReporter{Output: os.Stderr, Verbose: true}).Report(rch); err != nil {
		t.Fatalf("Error in test: %v", err)
	}
}

// TestOPA_Authorize_Basic tests that basic auth works using an extra provided
// module that implements "username from basic auth".
// TODO: write this extra module, depend on it in the core files?
func TestOPA_Authorize_Basic(t *testing.T) {
	ctx := context.Background()
	loader := ConstantLoader(
		WithConstantAdditionalModules(map[string]string{"extra-rules.rego": extraRegoTest}),
		WithConstantPermissionsYAML(`
users:
- name: auser
  queues:
  - prefix: /ns=auser/
    actions:
    - ALL
  - exact: /global/inbox
    actions:
    - INSERT
roles:
- name: "*"
  queues:
  - exact: /global/config
    actions:
    - READ
- name: admin
  queues:
  - prefix: ""
    actions:
    - ALL
`))
	opa, err := New(ctx, loader, WithInsecureTestUser())
	if err != nil {
		t.Fatalf("Error creating constant policy: %v", err)
	}
	defer opa.Close()

	req, err := authz.NewYAMLRequest(`
authz:
  testuser: "auser"
queues:
- exact: /ns=auser/myinbox
  actions:
  - CLAIM
`)
	if err != nil {
		t.Fatalf("Error parsing request yaml: %v", err)
	}

	if err := opa.Authorize(ctx, req); err != nil {
		t.Fatalf("Error authorizing: %v", err)
	}
}
