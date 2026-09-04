package eqk8s

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMeshClientReadsRotatingTokenForEveryPush(t *testing.T) {
	var mu sync.Mutex
	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/data/mesh" {
			t.Errorf("request = %s %s, want PUT /v1/data/mesh", r.Method, r.URL.Path)
		}
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	tokenFile := filepath.Join(t.TempDir(), "token")
	client := NewMeshClient(
		WithMeshURL(server.URL),
		WithAuthzTokenFile(tokenFile),
	)
	for _, token := range []string{"first-token", "second-token"} {
		if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := client.PushMesh(context.Background(), MeshDocument{Initialized: true}); err != nil {
			t.Fatalf("PushMesh: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"Bearer first-token", "Bearer second-token"}
	if len(authorizations) != len(want) {
		t.Fatalf("Authorization headers = %q, want %q", authorizations, want)
	}
	for i := range want {
		if authorizations[i] != want[i] {
			t.Fatalf("Authorization header %d = %q, want %q", i, authorizations[i], want[i])
		}
	}
}

func TestMeshClientLeavesAuthorizationUnsetWithoutTokenFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewMeshClient(WithMeshURL(server.URL))
	if err := client.PushMesh(context.Background(), MeshDocument{Initialized: true}); err != nil {
		t.Fatalf("PushMesh: %v", err)
	}
}

func TestMeshClientRejectsEmptyToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	client := NewMeshClient(WithMeshURL("http://unused"), WithAuthzTokenFile(tokenFile))
	if err := client.PushMesh(context.Background(), MeshDocument{Initialized: true}); err == nil {
		t.Fatal("PushMesh accepted an empty token")
	}
}
