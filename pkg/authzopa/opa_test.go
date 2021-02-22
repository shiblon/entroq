package authzopa

import (
	"context"
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
)

// TestCoreRules runs the OPA tests found in the file in this directory.
// These are the right way to test OPA in the raw.
func TestCoreRules(t *testing.T) {
	ctx := context.Background()

	coreMod, err := ast.ParseModule("core-rules.rego", coreRego)
	if err != nil {
		t.Fatalf("Failed to parse core module: %v", err)
	}

	testMod, err := ast.ParseModule("core-rules_test.rego", coreRegoTest)
	if err != nil {
		t.Fatalf("Failed to parse core test module: %v", err)
	}

	rch, err := tester.NewRunner().
		SetModules(map[string]*ast.Module{
			"core-rules.rego":      coreMod,
			"core-rules_test.rego": testMod,
		}).
		EnableFailureLine(true).
		EnableTracing(true).
		RunTests(ctx, nil)
	if err != nil {
		t.Fatalf("Error running tests: %v", err)
	}

	if err := (tester.PrettyReporter{Output: os.Stderr}).Report(rch); err != nil {
		t.Fatalf("Error in test: %v", err)
	}
}

// TestOPA_Authorize_Basic tests that basic auth works using an extra provided
// module that implements "username from basic auth".
// TODO: write this extra module, depend on it in the core files?
func TestOPA_Authorize_Basic(t *testing.T) {
	ctx := context.Background()
	opa, err := New(ctx, ConstantLoader(WithConstantPermissionsYAML(`
users:
- name: auser
  queues:
  - prefix: /ns=users/auser/
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
`)))
	if err != nil {
		t.Fatalf("Error creating constant policy: %v", err)
	}
	defer opa.Close()

	req, err := authz.NewYAMLRequest(`
authz:
  token: "auser"
queues:
- exact: /ns=users/auser/myinbox
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
