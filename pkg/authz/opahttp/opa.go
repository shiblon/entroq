// Package opahttp implements the authz.Authorizer using an Open Policy Agent (OPA).
package opahttp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/shiblon/entroq/pkg/authz"
)

const (
	DefaultHostURL = "http://localhost:8181"
	DefaultAPIPath = "/v1/data/entroq/authz"
)

// OPA is a client for interacting with a running OPA sidecar.
// It implements authz.Authorizer.
type OPA struct {
	hostURL string
	apiPath string
}

// Option configures an OPA authorizer.
type Option func(*OPA)

// WithHostURL sets the base URL of the OPA HTTP API.
func WithHostURL(u string) Option {
	return func(a *OPA) {
		if u != "" {
			a.hostURL = u
		}
	}
}

// WithAPIPath sets the policy path to query for authorization decisions.
func WithAPIPath(p string) Option {
	return func(a *OPA) {
		if p != "" {
			a.apiPath = p
		}
	}
}

// New creates a new OPA authorizer.
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
	return strings.TrimRight(h, "/") + "/" + strings.TrimLeft(p, "/")
}

// Authorize sends an authorization request to OPA. A nil error means allowed.
// If the error is an *authz.AuthzError it can be unpacked for details on which
// queues and actions were denied.
func (a *OPA) Authorize(ctx context.Context, req *authz.Request) error {
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
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("authorize: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("authorize read response: %w", err)
	}

	type authzResp struct {
		Result *authz.AuthzError `json:"result"`
	}
	result := new(authzResp)
	if err := json.NewDecoder(bytes.NewBuffer(respBytes)).Decode(result); err != nil {
		return fmt.Errorf("authorize decode response: %w", err)
	}

	if e := result.Result; !e.Allow {
		return e
	}

	return nil
}

// Close cleans up any resources used by this authorizer.
func (a *OPA) Close() error {
	return nil
}
