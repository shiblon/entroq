package mesh

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFileInstallsInitialPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.json")
	if err := os.WriteFile(path, []byte(validMeshJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a := New()
	if err := a.LoadFile(context.Background(), path); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !a.Ready() {
		t.Fatal("authorizer is not ready after loading policy file")
	}
	if err := a.Authorize(context.Background(), insertRequest(gatewaySubject, leafInbox)); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
}

func TestLoadFileRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.json")
	if err := os.WriteFile(path, []byte(`{"initialized":true,"surprise":true}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := New().LoadFile(context.Background(), path); err == nil {
		t.Fatal("LoadFile accepted an unknown field")
	}
}
