package opahttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shiblon/entroq/pkg/authz"
)

// newOPA points an OPA client at a test server, overriding the base URL while
// keeping the default API path (which the handler can ignore or inspect).
func newOPA(t *testing.T, handler http.HandlerFunc) *OPA {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(WithHostURL(srv.URL))
}

// TestAuthorizeDecisions covers how the client turns an OPA HTTP response into
// an allow (nil) or a deny (*authz.AuthzError). The undefined-decision and
// non-OK-status cases are the fail-closed guards: OPA answers an undefined
// policy path with "200 {}", which must deny rather than panic on a nil result.
func TestAuthorizeDecisions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr bool
		// check runs on the unpacked *authz.AuthzError (only when wantErr).
		check func(t *testing.T, e *authz.AuthzError)
	}{
		{
			name:    "allow",
			status:  http.StatusOK,
			body:    `{"result":{"allow":true}}`,
			wantErr: false,
		},
		{
			name:    "deny with failed queue",
			status:  http.StatusOK,
			body:    `{"result":{"allow":false,"failed":[{"exact":"q","actions":["CLAIM"]}]}}`,
			wantErr: true,
			check: func(t *testing.T, e *authz.AuthzError) {
				if len(e.Failed) != 1 || e.Failed[0].Exact != "q" {
					t.Errorf("failed queues = %v, want one entry for %q", e.Failed, "q")
				}
			},
		},
		{
			name:    "undefined decision fails closed",
			status:  http.StatusOK,
			body:    `{}`,
			wantErr: true,
			check: func(t *testing.T, e *authz.AuthzError) {
				if e.Allow {
					t.Error("undefined decision must not allow")
				}
			},
		},
		{
			name:    "null result fails closed",
			status:  http.StatusOK,
			body:    `{"result":null}`,
			wantErr: true,
		},
		{
			name:    "server error status fails closed",
			status:  http.StatusInternalServerError,
			body:    `{"code":"internal_error","message":"boom"}`,
			wantErr: true,
			check: func(t *testing.T, e *authz.AuthzError) {
				if e.Allow {
					t.Error("server error must not allow")
				}
			},
		},
		{
			name:    "forbidden status fails closed",
			status:  http.StatusForbidden,
			body:    `{}`,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newOPA(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body) //nolint:errcheck
			})

			err := a.Authorize(context.Background(), &authz.Request{
				Queues: []*authz.Queue{{Exact: "q", Actions: []authz.Action{authz.Claim}}},
			})

			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Authorize: got error %v, want allow (nil)", err)
				}
				return
			}

			if err == nil {
				t.Fatal("Authorize: got nil error, want a denial")
			}
			var e *authz.AuthzError
			if !errors.As(err, &e) {
				t.Fatalf("Authorize: error %v is not an *authz.AuthzError", err)
			}
			if tc.check != nil {
				tc.check(t, e)
			}
		})
	}
}

// TestAuthorizeSendsInputEnvelope verifies the client posts the request wrapped
// in an {"input": ...} envelope (OPA's Data API contract) to the configured
// path, carrying the claimant and queue actions the policy needs.
func TestAuthorizeSendsInputEnvelope(t *testing.T) {
	var gotPath string
	var gotBody map[string]*authz.Request

	a := newOPA(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		io.WriteString(w, `{"result":{"allow":true}}`) //nolint:errcheck
	})

	req := &authz.Request{
		ClaimantId: "worker-7",
		Queues:     []*authz.Queue{{Exact: "payments/inbox", Actions: []authz.Action{authz.Claim}}},
	}
	if err := a.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	if gotPath != DefaultAPIPath {
		t.Errorf("request path = %q, want default %q", gotPath, DefaultAPIPath)
	}
	in := gotBody["input"]
	if in == nil {
		t.Fatalf("request body had no %q envelope; got %v", "input", gotBody)
	}
	if in.ClaimantId != "worker-7" {
		t.Errorf("input.claimant_id = %q, want %q", in.ClaimantId, "worker-7")
	}
	if len(in.Queues) != 1 || in.Queues[0].Exact != "payments/inbox" {
		t.Errorf("input.queues = %v, want one entry for payments/inbox", in.Queues)
	}
}

// TestFullURL checks base-URL and path joining, including trailing/leading
// slash normalization and the empty-option fallbacks to the defaults.
func TestFullURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		host string
		path string
		want string
	}{
		{"defaults", "", "", DefaultHostURL + DefaultAPIPath},
		{"trailing slash on host", "http://opa:8181/", "/v1/data/x", "http://opa:8181/v1/data/x"},
		{"no leading slash on path", "http://opa:8181", "v1/data/x", "http://opa:8181/v1/data/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := New(WithHostURL(tc.host), WithAPIPath(tc.path))
			if got := a.fullURL(); got != tc.want {
				t.Errorf("fullURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
