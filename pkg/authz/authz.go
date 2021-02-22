// Package authz contains standard data structures for representing permissions and authorization requests / responses.
package authz // import "entrogo.com/entroq/pkg/authz"

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	yaml "gopkg.in/yaml.v3"
)

// Action is an authorization-style action that can be requested (and
// allowed or denied) for a particular queue spec.
type Action string

const (
	Claim  Action = "CLAIM"
	Delete Action = "DELETE"
	Change Action = "CHANGE"
	Insert Action = "INSERT"
	Read   Action = "READ"
	Any    Action = "ANY"
)

// Request conatins an authorization request to send to OPA.
type Request struct {
	// Authz contains information that came in with the request (headers).
	Authz *AuthzContext `json:"authz"`
	// Queues contains information about what is desired: what queues to
	// operate on, and what should be done to them.
	Queues []*QueueSpec `json:"queues"`
}

// NewYAMLRequest creates a request from YAML/JSON.
func NewYAMLRequest(y string) (*Request, error) {
	req := new(Request)
	if err := yaml.Unmarshal([]byte(y), req); err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	return req, nil
}

// AuthzContext represents per-request authz information. It can ideally come in many
// forms. The first supported form is a "token", such as from an Authorization
// header.
type AuthzContext struct {
	// Token contains the full value contents (omitting encoding and other
	// common header information) of an Authorization HTTP header, including
	// the type prefix (e.g., "Bearer")
	Token string `json:"token"`
}

// QueueSpec contains information about a single queue (it is expected that
// only one match string will be specified. Behavior of multiple specifications
// is not necessarily well defined, and depends on policy execution order.
type QueueSpec struct {
	// Exact match queue string.
	Exact string `json:"exact"`
	// Prefix match queue string.
	Prefix string `json:"prefix"`
	// Actions contains the desired things to be done with this queue.
	Actions []Action `json:"actions"`
}

// Permissions contains all allowed actions for every user and role in the system.
// TODO: do we need bindings between users and roles?
type Permissions struct {
	// Users contains actual individual information, for when specific
	// individuals need special overrides.
	Users []*Entity `json:"users"`

	// Roles contains allowances for groups.
	Roles []*Entity `json:"roles"`
}

// NewYAMLPermissions attempts to parse permissions as YAML, which is a superset of JSON, so can parse JSON, as well..
func NewYAMLPermissions(s string) (*Permissions, error) {
	p := new(Permissions)
	if err := yaml.Unmarshal([]byte(s), p); err != nil {
		return nil, fmt.Errorf("parse permissions: %w", err)
	}
	return p, nil
}

// String produces permissions in JSON format. Panics if it doesn't work (never expected).
func (p *Permissions) String() string {
	b, err := json.Marshal(p)
	if err != nil {
		log.Fatalf("Unable to produce JSON from permissions: %v", err)
	}
	return string(b)
}

// Entity contains information about a user or a role, and the things that
// user or role are allowed to do.
type Entity struct {
	// Name is the identifier for this user or role (not necessarily
	// human-friendly, more like a username).
	Name string `json:"name"`

	// Roles contains memberships for this entity (roles it is a member of).
	// It should be empty, and is ignored, when this entity represents a role.
	Roles []string `json:"roles"`

	// Queues contains information about what can be done to which queues.
	Queues []*QueueSpec `json:"queues"`
}

// AuthzError contains the reply from OPA, if non-empty. An empty UnmatchedQueues field implies
// that the action is allowed.
type AuthzError struct {
	// Failed contains the queue information for things that were not
	// found to be allowed by the policy. It will only contain the actions that
	// were not matched. If multiple actions were desired for a single queue,
	// only those disallowed are expected to be given back in the response.
	Failed []*QueueSpec `json:"failed"`
}

// Error satisfies the error interface, producing a string error that contains
// unmatched queue/action information.
func (e *AuthzError) Error() string {
	y, err := yaml.Marshal(e)
	if err != nil {
		return fmt.Sprintf("not authorized, failed to get data with reasons: %v", err)
	}
	return fmt.Sprintf("not authorized, missing queue/actions:\n%s", string(y))
}

// Authorizer is an abstraction over Rego policy. Provide one of these to
// manage policy files and changes. The query is expected to return a nil error
// when authorized, and a non-nil error when not authorized (or smoething else
// goes wrong). If the non-nil error is an AuthzError, it can be unpacked for
// information about which queues and actions were disallowed.
type Authorizer interface {
	// Authorize sends a request with context to see if something is allowed.
	// The request contains information about the entity making the request and
	// a nil error indicates that the request is permitted. A non-nil error can
	// be returned for system errors, or for permission denied reasons. The
	// latter will be of type AuthzError and can be detected in standard
	// errors.Is/As ways..
	Authorize(context.Context, *Request) error

	// Close cleans up any resources, or policy watchdogs, that the authorizer
	// might need in order to do its work.
	Close() error
}
