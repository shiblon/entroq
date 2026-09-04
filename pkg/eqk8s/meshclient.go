package eqk8s

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultMeshTimeout = 10 * time.Second

// MeshClient pushes mesh authorization documents to the active policy
// endpoint. The endpoint may be EntroQ's native authorizer or an OPA data API.
type MeshClient struct {
	url        string
	tokenFile  string
	httpClient *http.Client
}

// MeshClientOption configures a MeshClient.
type MeshClientOption func(*MeshClient)

// WithMeshURL sets the base URL of the mesh policy endpoint.
func WithMeshURL(url string) MeshClientOption {
	return func(c *MeshClient) {
		c.url = url
	}
}

// WithAuthzTokenFile sets a bearer-token file to read before every update.
// Reading per request handles Kubernetes projected-token rotation.
func WithAuthzTokenFile(path string) MeshClientOption {
	return func(c *MeshClient) {
		c.tokenFile = path
	}
}

// WithHTTPClient sets the HTTP client used for mesh policy requests. Use this
// to configure custom transports, mTLS, or test doubles.
func WithHTTPClient(httpClient *http.Client) MeshClientOption {
	return func(c *MeshClient) {
		c.httpClient = httpClient
	}
}

// NewMeshClient creates a MeshClient with the given options.
func NewMeshClient(options ...MeshClientOption) *MeshClient {
	c := &MeshClient{
		httpClient: &http.Client{Timeout: defaultMeshTimeout},
	}
	for _, option := range options {
		option(c)
	}
	return c
}

// PushMesh replaces the complete mesh authorization document.
func (c *MeshClient) PushMesh(ctx context.Context, mesh MeshDocument) error {
	body, err := json.Marshal(mesh)
	if err != nil {
		return fmt.Errorf("marshal mesh document: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPut,
		c.url+"/v1/data/mesh",
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("build mesh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.tokenFile != "" {
		token, err := os.ReadFile(c.tokenFile)
		if err != nil {
			return fmt.Errorf("read authorization token: %w", err)
		}
		value := strings.TrimSpace(string(token))
		if value == "" {
			return fmt.Errorf("read authorization token: token is empty")
		}
		req.Header.Set("Authorization", "Bearer "+value)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("PUT mesh document: %w", err)
	}
	statusCode := resp.StatusCode
	if err := resp.Body.Close(); err != nil {
		return fmt.Errorf("close mesh endpoint response: %w", err)
	}
	if statusCode != http.StatusNoContent {
		return fmt.Errorf("mesh endpoint returned unexpected status %d", statusCode)
	}

	return nil
}

// OPAClient is retained as a compatibility alias for MeshClient.
// Deprecated: use MeshClient.
type OPAClient = MeshClient

// OPAClientOption is retained as a compatibility alias for MeshClientOption.
// Deprecated: use MeshClientOption.
type OPAClientOption = MeshClientOption

// WithOPAURL is retained for compatibility with external OPA deployments.
// Deprecated: use WithMeshURL.
func WithOPAURL(url string) OPAClientOption {
	return WithMeshURL(url)
}

// NewOPAClient is retained for compatibility with external OPA deployments.
// Deprecated: use NewMeshClient.
func NewOPAClient(options ...OPAClientOption) *OPAClient {
	return NewMeshClient(options...)
}
