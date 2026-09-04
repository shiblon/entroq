package mesh

import (
	"context"
	"errors"
	"net/http"

	"github.com/shiblon/entroq/pkg/authn"
	"github.com/shiblon/entroq/pkg/authz/meshpolicy"
)

const (
	// MeshDataPath preserves the OPA data API path used by the eqk8s operator.
	MeshDataPath = "/v1/data/mesh"
	maxMeshBody  = 2 << 20
)

// MeshUpdater atomically replaces the active mesh authorization document.
type MeshUpdater interface {
	ReplaceMesh(context.Context, meshpolicy.Document) error
}

// NewMeshDataHandler returns the narrow authenticated subset of OPA's data API
// required by the eqk8s operator.
func NewMeshDataHandler(
	updater MeshUpdater,
	authenticator authn.Authenticator,
	allowedSubject string,
) (http.Handler, error) {
	if updater == nil {
		return nil, errors.New("mesh updater is required")
	}
	if authenticator == nil {
		return nil, errors.New("mesh update authenticator is required")
	}
	if allowedSubject == "" {
		return nil, errors.New("mesh update subject is required")
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.Header().Set("Allow", http.MethodPut)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		principal, err := authenticator.Authenticate(
			r.Context(),
			authn.NewHeaderCredentials(r.Header.Get("Authorization")),
		)
		if err != nil {
			var authErr *authn.Error
			if errors.As(err, &authErr) && authErr.Kind == authn.AuthenticationUnavailable {
				http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "authentication failed", http.StatusUnauthorized)
			return
		}
		if principal == nil {
			http.Error(w, "authentication returned no principal", http.StatusInternalServerError)
			return
		}
		if principal.Subject != allowedSubject {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		document, err := decodeDocument(http.MaxBytesReader(w, r.Body, maxMeshBody))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "mesh document too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "invalid mesh document", http.StatusBadRequest)
			return
		}
		if err := document.Validate(); err != nil {
			http.Error(w, "invalid mesh document", http.StatusBadRequest)
			return
		}
		if err := updater.ReplaceMesh(r.Context(), document); err != nil {
			http.Error(w, "replace mesh document", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}), nil
}
