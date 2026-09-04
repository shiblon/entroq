package mesh

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shiblon/entroq/pkg/authn"
	"github.com/shiblon/entroq/pkg/authz/meshpolicy"
)

const operatorSubject = "system:serviceaccount:eqk8s-system:eqk8s-controller-manager"

type stubAuthenticator struct {
	principal *authn.VerifiedPrincipal
	err       error
}

func (s *stubAuthenticator) Authenticate(context.Context, *authn.Credentials) (*authn.VerifiedPrincipal, error) {
	return s.principal, s.err
}

func (*stubAuthenticator) Close() error { return nil }

type stubUpdater struct {
	mesh meshpolicy.Document
	err  error
}

func (s *stubUpdater) ReplaceMesh(_ context.Context, mesh meshpolicy.Document) error {
	s.mesh = mesh
	return s.err
}

func TestNewMeshDataHandlerRequiresDependencies(t *testing.T) {
	goodUpdater := new(stubUpdater)
	goodAuthenticator := &stubAuthenticator{principal: &authn.VerifiedPrincipal{Subject: operatorSubject}}

	for _, tc := range []struct {
		name          string
		updater       MeshUpdater
		authenticator authn.Authenticator
		subject       string
	}{
		{name: "updater", authenticator: goodAuthenticator, subject: operatorSubject},
		{name: "authenticator", updater: goodUpdater, subject: operatorSubject},
		{name: "subject", updater: goodUpdater, authenticator: goodAuthenticator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewMeshDataHandler(tc.updater, tc.authenticator, tc.subject); err == nil {
				t.Fatal("NewMeshDataHandler succeeded with missing dependency")
			}
		})
	}
}

func TestMeshDataHandlerRequiresPUT(t *testing.T) {
	handler := newTestMeshHandler(t, new(stubUpdater), operatorSubject)
	req := httptest.NewRequest(http.MethodGet, MeshDataPath, nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPut {
		t.Fatalf("response = %d Allow=%q, want 405 Allow=PUT", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestMeshDataHandlerRequiresVerifiedOperator(t *testing.T) {
	for _, tc := range []struct {
		name          string
		authenticator *stubAuthenticator
		wantStatus    int
	}{
		{
			name: "invalid credentials",
			authenticator: &stubAuthenticator{
				err: authn.InvalidError("invalid", nil),
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "authentication unavailable",
			authenticator: &stubAuthenticator{
				err: authn.UnavailableError("unavailable", nil),
			},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:          "missing principal",
			authenticator: new(stubAuthenticator),
			wantStatus:    http.StatusInternalServerError,
		},
		{
			name: "wrong subject",
			authenticator: &stubAuthenticator{
				principal: &authn.VerifiedPrincipal{Subject: "somebody-else"},
			},
			wantStatus: http.StatusForbidden,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := NewMeshDataHandler(new(stubUpdater), tc.authenticator, operatorSubject)
			if err != nil {
				t.Fatalf("NewMeshDataHandler: %v", err)
			}
			req := httptest.NewRequest(http.MethodPut, MeshDataPath, strings.NewReader(validMeshJSON))
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func TestMeshDataHandlerReplacesDocument(t *testing.T) {
	updater := new(stubUpdater)
	handler := newTestMeshHandler(t, updater, operatorSubject)
	req := httptest.NewRequest(http.MethodPut, MeshDataPath, strings.NewReader(validMeshJSON))
	req.Header.Set("Authorization", "Bearer operator-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if !updater.mesh.Initialized || len(updater.mesh.Queues) != 1 || updater.mesh.Queues[0].Pattern != leafInbox {
		t.Fatalf("unexpected mesh document: %+v", updater.mesh)
	}
}

func TestMeshDataHandlerRejectsInvalidDocument(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"initialized":true,"unknown":true}`,
		validMeshJSON + `{}`,
	} {
		handler := newTestMeshHandler(t, new(stubUpdater), operatorSubject)
		req := httptest.NewRequest(http.MethodPut, MeshDataPath, strings.NewReader(body))
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
}

func TestMeshDataHandlerLimitsBody(t *testing.T) {
	handler := newTestMeshHandler(t, new(stubUpdater), operatorSubject)
	body := `{"initialized":true,"identities":{"` + strings.Repeat("x", maxMeshBody) + `":{}}}`
	req := httptest.NewRequest(http.MethodPut, MeshDataPath, strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestMeshDataHandlerReportsUpdateFailure(t *testing.T) {
	updater := &stubUpdater{err: errors.New("store failed")}
	handler := newTestMeshHandler(t, updater, operatorSubject)
	req := httptest.NewRequest(http.MethodPut, MeshDataPath, strings.NewReader(validMeshJSON))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func newTestMeshHandler(t *testing.T, updater MeshUpdater, subject string) http.Handler {
	t.Helper()
	handler, err := NewMeshDataHandler(
		updater,
		&stubAuthenticator{principal: &authn.VerifiedPrincipal{Subject: subject}},
		operatorSubject,
	)
	if err != nil {
		t.Fatalf("NewMeshDataHandler: %v", err)
	}
	return handler
}

const validMeshJSON = `{
  "initialized": true,
  "queues": [{
    "pattern": "/payments/leaf/inbox",
    "matchType": "Exact",
    "allowedCallers": [{"app": "gateway"}]
  }],
  "namespaces": [],
  "identities": {
    "system:serviceaccount:payments:gateway": {
      "labels": {"app": "gateway"}
    }
  }
}`
