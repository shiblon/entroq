package eqk8s

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const defaultOPATimeout = 10 * time.Second

// OPAClient pushes mesh authorization data to an OPA instance via its data API.
type OPAClient struct {
	url        string
	httpClient *http.Client
}

// OPAClientOption configures an OPAClient.
type OPAClientOption func(*OPAClient)

// WithOPAURL sets the base URL of the OPA instance.
func WithOPAURL(url string) OPAClientOption {
	return func(c *OPAClient) {
		c.url = url
	}
}

// WithHTTPClient sets the HTTP client used for OPA requests. Use this to
// configure custom transports, mTLS, or test doubles.
func WithHTTPClient(httpClient *http.Client) OPAClientOption {
	return func(c *OPAClient) {
		c.httpClient = httpClient
	}
}

// NewOPAClient creates an OPAClient with the given options.
func NewOPAClient(options ...OPAClientOption) *OPAClient {
	c := &OPAClient{
		httpClient: &http.Client{Timeout: defaultOPATimeout},
	}
	for _, o := range options {
		o(c)
	}
	return c
}

// PushMesh replaces the mesh authorization document at data.mesh in OPA.
func (c *OPAClient) PushMesh(ctx context.Context, mesh OPAMesh) error {
	body, err := json.Marshal(mesh)
	if err != nil {
		return fmt.Errorf("marshal mesh document: %w", err)
	}

	url := c.url + "/v1/data/mesh"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build OPA request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("PUT mesh document: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("OPA returned unexpected status %d", resp.StatusCode)
	}

	return nil
}
