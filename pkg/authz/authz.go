// Package authz contains standard data structures for representing permissions and authorization requests / responses.
package authz

import (
	"context"
	"errors"
	"fmt"
	"strings"

	yaml "gopkg.in/yaml.v2"
)

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

// Action is an authorization-style action that can be requested (and
// allowed or denied) for a particular queue spec.
type Action string

const (
	Claim  Action = "CLAIM"
	Delete Action = "DELETE"
	Change Action = "CHANGE"
	Insert Action = "INSERT"
	Read   Action = "READ"
	All    Action = "*"
)

// Request conatins an authorization request to send to OPA.
type Request struct {
	// Authz contains information that came in with the request (headers).
	Authz *Authorization `json:"authz"`
	// Queues contains information about what is desired: what queues to
	// operate on, and what should be done to them.
	Queues []*Queue `json:"queues,omitempty"`
	// Namespaces contains information about what is desired for document
	// storage: what namespaces to operate on, and what actions to take.
	Namespaces []*Namespace `json:"namespaces,omitempty"`
}

// NewYAMLRequest creates a request from YAML/JSON.
func NewYAMLRequest(y string) (*Request, error) {
	req := new(Request)
	if err := yaml.Unmarshal([]byte(y), req); err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	return req, nil
}

// Authorization represents per-request auth information extracted from the
// incoming request headers. The Type and Credentials fields correspond to the
// two parts of a standard HTTP Authorization header, e.g.:
//
//	Authorization: Bearer <jwt>
//	-> Type="Bearer", Credentials="<jwt>"
type Authorization struct {
	// Type is the scheme portion of the Authorization header (e.g. "Bearer").
	Type string `json:"type,omitempty"`
	// Credentials is the token or credentials portion of the Authorization header.
	Credentials string `json:"credentials,omitempty"`
}

// NewHeaderAuthorization creates an authorization structure from a header value.
func NewHeaderAuthorization(val string) *Authorization {
	pieces := strings.SplitN(val, " ", 2)
	az := new(Authorization)
	switch len(pieces) {
	case 1:
		az.Credentials = pieces[0]
	case 2:
		az.Type, az.Credentials = pieces[0], pieces[1]
	}
	return az
}

// String returns the authoriation header value.
func (a *Authorization) String() string {
	return strings.TrimSpace(strings.Join([]string{a.Type, a.Credentials}, " "))
}

// Queue contains information about a single queue (it is expected that
// only one match string will be specified. Behavior of multiple specifications
// is not necessarily well defined, and depends on policy execution order.
type Queue struct {
	// An exact name to match.
	Exact string `yaml:",omitempty" json:"exact,omitempty"`
	// The kind of matching to do (default exact)
	Prefix string `yaml:",omitempty" json:"prefix,omitempty"`
	// Actions contains the desired things to be done with this queue.
	Actions []Action `yaml:",flow" json:"actions"`
}

// Namespace contains information about a single namespace. Like queues,
// it supports exact or prefix matching.
type Namespace struct {
	// An exact name to match.
	Exact string `yaml:",omitempty" json:"exact,omitempty"`
	// The kind of matching to do (default exact)
	Prefix string `yaml:",omitempty" json:"prefix,omitempty"`
	// Actions contains the desired things to be done with this namespace.
	Actions []Action `yaml:",flow" json:"actions"`
}

// String produces a simple string representing this queue spec.
func (q *Queue) String() string {
	vals := []string{
		fmt.Sprint(q.Actions),
	}
	if q.Exact != "" {
		vals = append(vals, fmt.Sprintf("q:%s", q.Exact))
	}
	if q.Prefix != "" {
		vals = append(vals, fmt.Sprintf("p:%s", q.Prefix))
	}

	return strings.Join(vals, " ")
}

// AuthzError contains the reply from OPA.
type AuthzError struct {
	// If Allow is true, then authorization succeeded and we can proceed.
	// The reason we don't just go with "empty error and failures" is that
	// non-affirmative things like that tend to cause unwanted authorizations
	// for other reasons, like parsing JSON with no known fields present.
	Allow bool `json:"allow"`

	// Failed contains the queue information for things that were not
	// found to be allowed by the policy. It will only contain the actions that
	// were not matched. If multiple actions were desired for a single queue,
	// only those disallowed are expected to be given back in the response.
	Failed []*Queue `json:"failed"`

	// FailedNamespaces contains namespace information for things that were
	// not allowed by policy.
	FailedNamespaces []*Namespace `json:"failed_namespaces"`

	// For other kinds of errors.
	Errors []string `json:"errors"`
}

// Error satisfies the error interface, producing a string error that contains
// unmatched queue/action information.
func (e *AuthzError) Error() string {
	if e.Allow {
		return "authorization OK"
	}
	var vals []string
	if len(e.Failed) != 0 {
		vals = append(vals, fmt.Sprint(e.Failed))
	}
	if len(e.Errors) != 0 {
		vals = append(vals, fmt.Sprint(e.Errors))
	}
	return fmt.Sprintf("authorization failed: %s", strings.Join(vals, "; "))
}

// IsAuthz reports whether err is (or wraps) an *AuthzError.
func IsAuthz(err error) bool {
	var e *AuthzError
	return errors.As(err, &e)
}
