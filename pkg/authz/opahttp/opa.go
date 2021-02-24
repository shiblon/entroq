// Package opahttp implements the authz.Authorizer using an Open Policy Agent (OPA).
package opahttp // import "entrogo.com/entroq/pkg/authz/opahttp"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"entrogo.com/entroq/pkg/authz"
)

// OPA is a client-like object for interacting with OPA authorization policies.
// It adheres to the authz.Authorizer interface.
type OPA struct {
	apiURL        *url.URL
	allowTestUser bool
}

// Option defines a setting for creating an OPA authorizer.
type Option func(*OPA) error

// WithInsecureTestUser must be set when doing testing and the use of the
// Authz.TestUser (instead of a signed token, for example) is desired. Without
// this option, the presence of the TestUser field causes an error.
func WithInsecureTestUser() Option {
	return func(a *OPA) error {
		a.allowTestUser = true
		return nil
	}
}

// WithBaseURL sets the OPA base URL (scheme, host, port) to which v1 queries
// should go, e.g., http://localhost:9020.
func WithBaseURL(u string) Option {
	return func(a *OPA) error {
		base, err := url.Parse(u)
		if err != nil {
			return fmt.Errorf("set base URL: %w", err)
		}
		base.Path = "/v1/data/entroq/authz/queues_result"
		a.apiURL = base
		return nil
	}
}

// New creates a new OPA client with the given options.
func New(opts ...Option) (*OPA, error) {
	a := new(OPA)
	for _, opt := range opts {
		if err := opt(a); err != nil {
			return nil, fmt.Errorf("new opa: %w", err)
		}
	}

	if a.apiURL == nil {
		return nil, fmt.Errorf("new opa has no base URL")
	}

	return a, nil
}

// Authorize checks for unmatched queues and actions. A nil error means authorized.
// If the error satisfies errors.Is on a *authz.AuthzError, it can be unpacked to find
// which queues and actions were not satisfied.
func (a *OPA) Authorize(ctx context.Context, req *authz.Request) error {
	if !a.allowTestUser && req.Authz.TestUser != "" {
		return fmt.Errorf("insecure test user present, but not allowed")
	}

	body := map[string]*authz.Request{
		"input": req,
	}

	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("authorize: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiURL.String(), bytes.NewBuffer(b))
	if err != nil {
		return fmt.Errorf("authorize: %w", err)
	}
	httpReq.Header.Add("Content-Type", "application/json")
	httpReq.Header.Add("Authorization", req.Authz.String())

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("authorize: %w", err)
	}
	defer resp.Body.Close()

	result := map[string]*authz.AuthzError{
		"result": new(authz.AuthzError),
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("authorize: %w", err)
	}

	// Check result value.
	if e := result["result"]; len(e.Failed) != 0 {
		// We got an error with information about missing queue/actions.
		return e
	}

	return nil
}
