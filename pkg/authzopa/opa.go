// Package authzopa implements the authz.Authorizer using an Open Policy Agent (OPA).
package authzopa // import "entrogo.com/entroq/pkg/authzopa"

import (
	"context"
	"encoding/json"
	"fmt"

	"entrogo.com/entroq/pkg/authz"
	"github.com/open-policy-agent/opa/rego"
)

// OPA is a client-like object for interacting with OPA authorization policies.
// It adheres to the authz.Authorizer interface.
type OPA struct {
	policy Policy
}

// Policy provides the ability to get partial results, suitable for running OPA
// authorization queries with new input.
type Policy interface {
	// PreparedQuery produces a Rego partial result, which has everything ready to go
	// and is simply awaiting input for eval.
	PreparedQuery(context.Context) (rego.PreparedEvalQuery, error)
	// Close cleans up the policy, which might have file watchers and other
	// resources held open.
	Close() error
}

// PolicyLoader can be used to create a policy.
type PolicyLoader func(context.Context) (Policy, error)

// New creates a new OPA with the given options. A policy loader must be
// specified so that it can do its work. The OPA value should be closed when no
// longer in use.
func New(ctx context.Context, loader PolicyLoader) (*OPA, error) {
	p, err := loader(ctx)
	if err != nil {
		return nil, fmt.Errorf("new opa: %w", err)
	}

	return &OPA{
		policy: p,
	}, nil
}

// Close shuts down policy refresh watchers, and releases OPA resources. Should
// be called if the OPA does not share a life cycle with the main process.
func (a *OPA) Close() error {
	return a.policy.Close()
}

// Authorize checks for unmatched queues and actions. A nil error means authorized.
func (a *OPA) Authorize(ctx context.Context, req *authz.Request) error {
	prep, err := a.policy.PreparedQuery(ctx)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	results, err := prep.Eval(ctx, rego.EvalInput(req))
	if err != nil {
		return fmt.Errorf("authorize opa: %w", err)
	}

	exprs := results[0].Expressions

	if len(exprs) != 1 {
		return fmt.Errorf("authorize: empty result expression")
	}

	val := exprs[0].Value.(map[string]interface{})

	// Convert to JSON, so we can create an error if needed.
	b, err := json.Marshal(val)
	if err != nil {
		return fmt.Errorf("authz response marshal: %v", err)
	}

	e := new(authz.AuthzError)
	if err := json.Unmarshal(b, e); err != nil {
		return fmt.Errorf("authz convert to useful error: %v\njson=%v", err, string(b))
	}

	if len(e.Failed) != 0 {
		return e
	}
	return nil
}
