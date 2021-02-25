// Package opahttp implements the authz.Authorizer using an Open Policy Agent (OPA).
package opahttp // import "entrogo.com/entroq/pkg/authz/opahttp"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"entrogo.com/entroq/pkg/authz"
)

const (
	DefaultHostURL = "http://localhost:8181"
	DefaultAPIPath = "/v1/data/entroq/authz/queues_result"
)

// OPA is a client-like object for interacting with OPA authorization policies.
// It adheres to the authz.Authorizer interface.
type OPA struct {
	hostURL       string
	apiPath       string
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

// WithHostURL sets the host OPA URL for a query authorization request, such as
// its default value given in DefaultURL.
func WithHostURL(u string) Option {
	return func(a *OPA) {
		if u != "" {
			a.hostURL = u
		}
	}
}

// WithAPIPath sets the API path to request for authorization.
func WithAPIPath(p string) Option {
	return func(a *OPA) {
		if p != "" {
			a.apiPath = p
		}
	}
}

// New creates a new OPA client with the given options.
func New(opts ...Option) *OPA {
	a := &OPA{
		hostURL: DefaultHostURL,
		apiPath: DefaultAPIPath,
	}
	for _, opt := range opts {
		opt(a)
	}

	return a
}

func (a *OPA) fullURL() string {
	h, p := a.hostURL, a.apiPath
	if h == "" {
		h = DefaultHostURL
	}
	if p == "" {
		p = DefaultAPIPath
	}
	return strings.TrimRight(h, "/") + "/" + p
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

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.fullURL(), bytes.NewBuffer(b))
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

// Close cleans up any resources used.
func (a *OPA) Close() error {
	return nil
}
