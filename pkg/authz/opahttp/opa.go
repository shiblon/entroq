// Package opahttp implements the authz.Authorizer using an Open Policy Agent (OPA).
package opahttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/shiblon/entroq/pkg/authz"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

const (
	DefaultHostURL = "http://localhost:8181"
	DefaultAPIPath = "/v1/data/entroq/authz"
)

// OPA is a client for interacting with a running OPA sidecar.
// It implements authz.Authorizer.
type OPA struct {
	hostURL  string
	apiPath  string
	mp       metric.MeterProvider
	duration metric.Float64Histogram
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

// WithMeterProvider records client-observed OPA request duration. This includes
// HTTP transport and scheduling time that OPA's own handler metric cannot see.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(a *OPA) {
		if mp != nil {
			a.mp = mp
		}
	}
}

// New creates a new OPA authorizer.
func New(opts ...Option) *OPA {
	a := &OPA{
		hostURL: DefaultHostURL,
		apiPath: DefaultAPIPath,
		mp:      noop.NewMeterProvider(),
	}
	for _, opt := range opts {
		opt(a)
	}
	a.duration, _ = a.mp.Meter("entroq/authz/opahttp").Float64Histogram(
		"authz.opa.duration_seconds",
		metric.WithDescription("Client-observed OPA authorization request duration."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(
			0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01,
			0.025, 0.05, 0.1, 0.25, 0.5, 1,
		),
	)
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
func (a *OPA) Authorize(ctx context.Context, req *authz.Request) (resultErr error) {
	started := time.Now()
	defer func() {
		outcome := "allow"
		if resultErr != nil {
			outcome = "error"
			var authzErr *authz.AuthzError
			if errors.As(resultErr, &authzErr) {
				outcome = "deny"
			}
		}
		if a.duration != nil {
			a.duration.Record(ctx, time.Since(started).Seconds(),
				metric.WithAttributes(
					attribute.String("actions", actionSet(req)),
					attribute.String("outcome", outcome),
				))
		}
	}()
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

	// A non-OK status means OPA itself errored (bad request, wrong path, server
	// fault) rather than rendering a decision. Fail closed instead of trying to
	// interpret the body as an allow/deny.
	if resp.StatusCode != http.StatusOK {
		return &authz.AuthzError{
			Errors: []string{fmt.Sprintf("OPA returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))},
		}
	}

	type authzResp struct {
		Result *authz.AuthzError `json:"result"`
	}
	result := new(authzResp)
	if err := json.NewDecoder(bytes.NewBuffer(respBytes)).Decode(result); err != nil {
		return fmt.Errorf("authorize decode response: %w", err)
	}

	// OPA answers an undefined decision with "200 {}" (no result field) -- e.g.
	// a policy path that names no rule, or a rule that never sets a value. There
	// is no decision to honor, so fail closed rather than dereferencing a nil
	// result (which would panic) or treating absence as permission.
	if result.Result == nil {
		return &authz.AuthzError{
			Errors: []string{fmt.Sprintf("OPA returned no decision at %s (undefined policy path?)", a.fullURL())},
		}
	}

	if e := result.Result; !e.Allow {
		return e
	}

	return nil
}

// actionSet returns a bounded-cardinality description of the requested
// operations. Queue and namespace names are deliberately excluded from metric
// attributes.
func actionSet(req *authz.Request) string {
	if req == nil {
		return "none"
	}
	actions := make(map[string]struct{})
	add := func(values []authz.Action) {
		for _, action := range values {
			actions[string(action)] = struct{}{}
		}
	}
	for _, queue := range req.Queues {
		if queue != nil {
			add(queue.Actions)
		}
	}
	for _, namespace := range req.Namespaces {
		if namespace != nil {
			add(namespace.Actions)
		}
	}
	if len(actions) == 0 {
		return "none"
	}
	values := make([]string, 0, len(actions))
	for action := range actions {
		values = append(values, action)
	}
	sort.Strings(values)
	return strings.Join(values, "+")
}

// Close cleans up any resources used by this authorizer.
func (a *OPA) Close() error {
	return nil
}
