// Package opa implements utilities for doing authorization using the Open
// Policy Agent (OPA). It uses the authz proto definitions.
package opa

import (
	"bytes"
	"context"
	"fmt"

	"github.com/open-policy-agent/opa/rego"
)

// Authz represents per-request authz information. It can ideally come in many
// forms. The first supported form is a "token", such as from an Authorization
// header.
type Authz struct {
	// Token contains the full value contents (omitting encoding and other
	// common header information) of an Authorization HTTP header, including
	// the type prefix (e.g., "Bearer")
	Token string `json:"token"`
}

// QueueAuthz contains information about a single queue (it is expected that
// only one match string will be specified, though multiple are technically
// allowed).
type QueueAuthz struct {
	// Exact match queue string.
	Exact string `json:"exact"`
	// Prefix match queue string.
	Prefix string `json:"prefix"`
	// Actions contains the desired things to be done with this queue.
	// TODO: make this an enum.
	Actions []string `yaml:",flow" json:"actions"`
}

// AuthzRequest conatins an authorization request to send to OPA.
type AuthzRequest struct {
	// Authz contains information that came in with the request (headers).
	Authz *Authz `json:"authz"`
	// Queues contains information about what is desired: what queues to
	// operate on, and what should be done to them.
	Queues []*QueueAuthz `json:"queues"`
}

// AuthzResponse contains the reply from OPA. An empty UnmatchedQueues field implies
// that the action is allowed.
type AuthzResponse struct {
	// UnmatchedQueues contains the queue information for things that were not
	// found to be allowed. It will only contain the actions that were not
	// matched. If multiple actions were desired for a single queue, only those
	// disallowed are expected to be given back in the response.
	UnmatchedQueues []*QueueAuthz `json:"queues"`
}

// AuthzDependencyError is a special type of error holding an AuthzResponse.
type AuthzDependencyError AuthzResponse

// Error satisfies the error interface.
func (e *AuthzDependencyError) Error() string {
	resp := (*AuthzResponse)(e)

	y, err := yaml.Marshal(resp)
	if err != nil {
		return fmt.Sprintf("not authorized, failed to get data with reasons: %v", err)
	}

	return fmt.Sprintf("not authorized, missing queue/actions:\n%s", string(y))
}

// AuthzEntity contains information about a user or a role, and the things that
// user or role are allowed to do.
type AuthzEntity struct {
	// Name is the identifier for this user or role (not necessarily
	// human-friendly, more like a username).
	Name string `yaml:"name" json:"name"`

	// Roles contains memberships for this entity (roles it is a member of).
	// It should be empty, and is ignored, when this entity represents a role.
	Roles []string `yaml:"roles" json:"roles"`

	// Queues contains information about what can be done to which queues.
	Queues []*QueueAuthz `yaml:"queues" json:"queues"`
}

// AuthzPermissions contains all allowed actions for every user and role in the system.
type AuthzPermissions struct {
	// Users contains actual individual information, for when specific
	// individuals need special overrides.
	Users []*AuthzEntity `yaml:"users" json:"users"`

	// Roles contains allowances for groups.
	Roles []*AuthzEntity `yaml:"roles" json:"roles"`
}

// OPA is a client-like object for interacting with OPA authorization policies.
type OPA struct {
	impl Authorizer
}

// New creates a new OPA with the given options. A policy should be specified
// so that it can do its work. This should be closed when no longer in use.
func New(authorizer Authorizer) *OPA {
	return &OPA{
		impl: authorizer,
	}
}

// Close shuts down policy refresh watchers, and releases OPA resources. Should
// be called if the OPA does not share a life cycle with the main process.
func (a *OPA) Close() error {
	if a.cancelWatcher != nil {
		a.cancelWatcher()
	}
	return nil
}

// Authorizer is an abstraction over Rego policy. Provide one of these to
// manage policy files and changes. The query is expected to return a nil error
// when authorized, and a non-nil error when not authorized (or smoething else
// goes wrong). // TODO: specify the type of unauthorized error.
type Authorizer interface {
	Authorize(context.Context, *AuthzRequest) error
	Close() error
}

// StringAuthorizer keeps OPA policies as strings, and they are expected to never change.
type StringAuthorizer struct {
	EntitiesJSON string            // Entities JSON (must be JSON, not YAML).
	RuleModules  map[string]string // Map from package name to Rego contents.

	impl *rego.Rego
	pr   *rego.PartialResult
}

// NewStringAuthorizer creates a valid policy authorize from various string contents:
// - a query for the policy (e.g., "failed_queues"), expected to return content in the format of the AuthzResponse.
// - a JSON string containing entity information shaped like AuthzEntities, and
// - a map of module strings in Rego.
func NewStringAuthorizer(query string, entitiesJSON string, rules map[string]string) *PolicyStringManager {
	options := []func(*rego.Rego){
		rego.Query(query),
		rego.Store(inmem.NewFromReader(bytes.NewBufferString(entitiesJSON))),
	}

	for name, module := range rules {
		options = append(options, rego.Module(name, module))
	}

	return &PolicyStringManager{
		EntitiesJSON: entitiesJSON,
		RuleModules:  rules,
		impl:         rego.New(options...),
	}
}

func (a *StringAuthorizer) partialResult(ctx context.Context) (*rego.PartialResult, error) {
	if m.pr != nil {
		return m.pr, nil
	}
	var err error
	m.pr, err = m.impl.PartialResult(ctx)
	return m.pr, err
}

// Query checks for unmatched queues and actions. A nil error means authorized.
func (a *StringAuthorizer) Authorize(ctx context.Context, req *AuthzRequest) error {
	pr, err := m.partialResult()
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}

	r := pr.Rego(rego.Input(req))
	results, err := r.Partial(ctx)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}

	// TODO: package up any non-empty results into a suitable error.

	// Nothing in failed queues, we're good.
	return nil
}

// Authorize returns an appropriate Authz Response based on information in the
// context, OPA policy, and request inputs. If there is no error, the request
// is authorized.
func (a *OPA) Authorize(ctx context.Context, req *AuthzRequest) error {
	if err := a.impl.Authorize(ctx, req); err != nil {
		return fmt.Errorf("authorize: %w", err)
	}
}
