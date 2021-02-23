package authzopa

import (
	"bytes"
	"context"
	"fmt"
	"log"

	"entrogo.com/entroq/pkg/authz"
	"github.com/open-policy-agent/opa/rego"
	"github.com/open-policy-agent/opa/storage/inmem"

	_ "embed"
)

var (
	//go:embed core-rules.rego
	coreRego string

	//go:embed queues.rego
	queuesRego string
)

// Constant creates OPA mechanisms from constant string values (rules + data) that never change.
type Constant struct {
	preparedQuery rego.PreparedEvalQuery

	// OPA query for this policy.
	query string

	// Only one of permsYAML (also allows JSON) and perms will need to be specified.
	perms *authz.Permissions

	// OPA Rego module strings.
	modules map[string]string
}

// ConstantLoader returns a Constant loader for use in OPA.New.
func ConstantLoader(opts ...ConstantOption) PolicyLoader {
	return func(ctx context.Context) (Policy, error) {
		p, err := NewConstant(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("load canstant: %w", err)
		}
		return p, nil
	}
}

// ConstantOption is an option for creating a constant policy.
type ConstantOption func(*Constant) error

// WithConstantPermissions sets the permissions for this constant policy.
func WithConstantPermissions(perms *authz.Permissions) ConstantOption {
	return func(p *Constant) error {
		p.perms = perms
		return nil
	}
}

// WithConstantPermissionsYAML sets the permissions from a YAML/JSON string.
func WithConstantPermissionsYAML(y string) ConstantOption {
	return func(p *Constant) error {
		perms, err := authz.NewYAMLPermissions(y)
		if err != nil {
			return fmt.Errorf("constant perms yaml: %w", err)
		}
		p.perms = perms
		return nil
	}
}

// WithConstantAdditionalModules adds modules to the core module (which is not optional).
// These are merged into the full modules map, including the core module. If
// one of these has the same name as the CoreModuleName, it will be ignored.
func WithConstantAdditionalModules(modules map[string]string) ConstantOption {
	return func(p *Constant) error {
		for k, v := range modules {
			p.modules[k] = v
		}
		return nil
	}
}

// NewConstant creates a constant policy, ready to authorize.
func NewConstant(ctx context.Context, opts ...ConstantOption) (*Constant, error) {
	cp := &Constant{
		modules: map[string]string{
			"core-rules.rego": coreRego,
			"queues.rego":     queuesRego,
		},
	}

	for _, opt := range opts {
		if err := opt(cp); err != nil {
			return nil, fmt.Errorf("new constant option: %w", err)
		}
	}

	// Create Rego options for a result.
	options := []func(*rego.Rego){
		rego.Query(`{"user": data.entroq.authz.username, "failed": data.entroq.authz.failed_queues}`),
	}

	if cp.perms != nil {
		options = append(options, rego.Store(inmem.NewFromReader(bytes.NewBufferString(cp.perms.String()))))
	} else {
		log.Print("WARNING: No permissions specified, only global policy will be applied.")
	}

	for name, module := range cp.modules {
		options = append(options, rego.Module(name, module))
	}

	var err error
	if cp.preparedQuery, err = rego.New(options...).PrepareForEval(ctx); err != nil {
		return nil, fmt.Errorf("new constant prep: %w", err)
	}

	return cp, nil
}

// PreparedQuery creates a prepared query given the data, policy, and query. Returns a
// Rego result suitable for passing in new input and getting results back.
func (c *Constant) PreparedQuery(ctx context.Context) (rego.PreparedEvalQuery, error) {
	return c.preparedQuery, nil
}

// Close closes the constant policy.
func (*Constant) Close() error {
	return nil
}
