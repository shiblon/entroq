package authzopa

import (
	"context"
	"testing"

	_ "embed"

	"entrogo.com/entroq/pkg/authz"
)

//go:embed core-rules_test.rego
var coreRegoTest string

func TestOPA_Authorize_Basic(t *testing.T) {
	ctx := context.Background()
	opa, err := New(ctx, ConstantLoader(WithConstantPermissionsYAML(`
users:
- name: auser
  queues:
  - prefix: /ns=users/chris/
    actions:
    - ANY
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
    - ANY
`)))
	if err != nil {
		t.Fatalf("Error creating constant policy: %v", err)
	}
	defer opa.Close()

	req, err := authz.NewYAMLRequest(`
authz:
  token: "User auser"
queues:
- exact: /ns=users/chris/myinbox
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
