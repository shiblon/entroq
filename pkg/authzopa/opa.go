// Package authzopa implements the authz.Authorizer using an Open Policy Agent (OPA).
package authzopa // import "entrogo.com/entroq/pkg/authzopa"

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
	baseURL       *url.URL
	allowTestUser bool
}

// Option defines a setting for creating an OPA authorizer.
type Option func(*OPA)

// WithInsecureTestUser must be set when doing testing and the use of the
// Authz.TestUser (instead of a signed token, for example) is desired. Without
// this option, the presence of the TestUser field causes an error.
func WithInsecureTestUser() Option {
	return func(a *OPA) {
		a.allowTestUser = true
	}
}

// New creates a new OPA client with the given URL and options.
// The base URL should contain only the scheme and the host:port, e.g.,
// http://localhost:9020.
func New(opaBaseURL string, opts ...Option) (*OPA, error) {
	u, err := url.Parse(opaBaseURL)
	if err != nil {
		return nil, fmt.Errorf("new opa: %w", err)
	}
	a := &OPA{
		baseURL: u,
	}

	for _, opt := range opts {
		opt(a)
	}

	return a, nil
}

// Authorize checks for unmatched queues and actions. A nil error means authorized.
func (a *OPA) Authorize(ctx context.Context, req *authz.Request) error {
	if !a.allowTestUser && req.Authz.TestUser != "" {
		return fmt.Errorf("insecure test user present, but not allowed")
	}

	fullURL := new(url.URL)
	*fullURL = *a.baseURL
	fullURL.Path = "/v1/data/entroq/authz/result"

	body := map[string]*authz.Request{
		"input": req,
	}

	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("authorize: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL.String(), bytes.NewBuffer(b))
	if err != nil {
		return fmt.Errorf("authorize: %w", err)
	}
	httpReq.Header.Add("Content-Type", "application/json")

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
