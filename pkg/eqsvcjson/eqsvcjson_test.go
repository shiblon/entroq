package eqsvcjson_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/eqsvcgrpc"
	"github.com/shiblon/entroq/pkg/eqsvcjson"
)

// newTestServer stands up an in-memory EntroQ backend behind the JSON/Connect
// handler and returns an httptest.Server plus a cleanup function.
func newTestServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	svc, err := eqsvcgrpc.New(context.Background(), eqmem.Opener())
	if err != nil {
		t.Fatalf("new svc: %v", err)
	}
	_, handler, err := eqsvcjson.New(svc)
	if err != nil {
		svc.Close()
		t.Fatalf("new handler: %v", err)
	}
	ts := httptest.NewServer(handler)
	return ts, func() { ts.Close(); svc.Close() }
}

func postJSON(t *testing.T, url string, body any) (int, map[string]any) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("decode response %q: %v", data, err)
		}
	}
	return resp.StatusCode, out
}

// TestModifyDependencyErrorIs409 guards the JSON adapter's error translation:
// a dependency error from the gRPC QSvc (a grpc/status NotFound) must surface
// as HTTP 409 Conflict carrying flat ModifyDep details, not collapse to a
// detail-less 500. See translateErr in eqsvcjson.go.
func TestModifyDependencyErrorIs409(t *testing.T) {
	ts, cleanup := newTestServer(t)
	defer cleanup()
	modifyURL := ts.URL + "/api/v0/modify"

	code, out := postJSON(t, modifyURL, map[string]any{
		"claimantId": "test",
		"inserts":    []map[string]any{{"queue": "/q", "value": "x"}},
	})
	if code != http.StatusOK {
		t.Fatalf("insert status = %d, want 200; body=%v", code, out)
	}
	inserted, ok := out["inserted"].([]any)
	if !ok || len(inserted) != 1 {
		t.Fatalf("expected one inserted task, got %v", out)
	}
	id, _ := inserted[0].(map[string]any)["id"].(string)
	if id == "" {
		t.Fatalf("inserted task missing id: %v", out)
	}

	// Delete at a version that never existed: a dependency error.
	code, out = postJSON(t, modifyURL, map[string]any{
		"claimantId": "test",
		"deletes":    []map[string]any{{"id": id, "version": 999, "queue": "/q"}},
	})
	if code != http.StatusConflict {
		t.Fatalf("dependency status = %d, want 409 Conflict; body=%v", code, out)
	}

	details, ok := out["details"].([]any)
	if !ok || len(details) == 0 {
		t.Fatalf("expected dependency details, got %v", out)
	}
	foundDelete := false
	for _, d := range details {
		dm, _ := d.(map[string]any)
		if dm["type"] != "DELETE" {
			continue
		}
		foundDelete = true
		idObj, _ := dm["id"].(map[string]any)
		if got, _ := idObj["id"].(string); got != id {
			t.Errorf("DELETE detail id = %q, want %q", got, id)
		}
	}
	if !foundDelete {
		t.Errorf("expected a flat DELETE ModifyDep detail, got %v", details)
	}
}
