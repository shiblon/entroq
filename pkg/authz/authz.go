// Package authz contains standard data structures for representing permissions and authorization requests / responses.
package authz // import "entrogo.com/entroq/pkg/authz"

import (
	"context"
	"fmt"

	yaml "gopkg.in/yaml.v2"
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
	All    Action = "ALL"
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
	// An HTTP Authorization header is split into its type and credentials and
	// included here when available.
	Type        string `json:"type"`
	Credentials string `json:"credentials"`

	// Never use this in practice. This allows the user to be set directly for testing.
	// The code checks for this and creates an error if present, unless
	// specifically allowed for testing.
	TestUser string `json:"testuser"`
}

// QueueSpec contains information about a single queue (it is expected that
// only one match string will be specified. Behavior of multiple specifications
// is not necessarily well defined, and depends on policy execution order.
type QueueSpec struct {
	// Exact match queue string.
	Exact string `yaml:",omitempty" json:"exact,omitempty"`
	// Prefix match queue string.
	Prefix string `yaml:",omitempty" json:"prefix,omitempty"`
	// Actions contains the desired things to be done with this queue.
	Actions []Action `yaml:",flow" json:"actions"`
}

// AuthzError contains the reply from OPA, if non-empty. An empty UnmatchedQueues field implies
// that the action is allowed.
type AuthzError struct {
	User string `json:"user"`
	// Failed contains the queue information for things that were not
	// found to be allowed by the policy. It will only contain the actions that
	// were not matched. If multiple actions were desired for a single queue,
	// only those disallowed are expected to be given back in the response.
	Failed []*QueueSpec `json:"failed"`
}

// Error satisfies the error interface, producing a string error that contains
// unmatched queue/action information.
func (e *AuthzError) Error() string {
	y, err := yaml.Marshal(e.Failed)
	if err != nil {
		return fmt.Sprintf("user %q not authorized, failed to get data with reasons: %v", e.User, err)
	}
	return fmt.Sprintf("user %q not authorized, missing queue/actions:\n%s", e.User, string(y))
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
