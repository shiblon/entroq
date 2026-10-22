package eqsvcjson_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/api/apiconnect"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/eqsvcgrpc"
	"github.com/shiblon/entroq/pkg/eqsvcjson"
	"github.com/shiblon/entroq/pkg/version"
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

// TestOverLimitValuesAre400 checks that a value over its length limit is
// reported as a bad request, not a server error: the backend returns an
// entroq.InvalidArgumentError, which must survive as InvalidArgument.
func TestOverLimitValuesAre400(t *testing.T) {
	ts, cleanup := newTestServer(t)
	defer cleanup()

	long := strings.Repeat("c", 65)
	tests := []struct {
		name string
		path string
		body map[string]any
	}{
		{"claim claimant", "/api/v0/claim", map[string]any{
			"claimantId": long,
			"queues":     []string{"/q"},
			"durationMs": 1000,
		}},
		{"modify claimant", "/api/v0/modify", map[string]any{
			"claimantId": long,
			"inserts":    []map[string]any{{"queue": "/q"}},
		}},
		{"task id", "/api/v0/modify", map[string]any{
			"claimantId": "test",
			"inserts":    []map[string]any{{"queue": "/q", "id": long}},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, out := postJSON(t, ts.URL+test.path, test.body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%v", code, out)
			}
		})
	}
}

// TestResponsesCarryProtocol checks that JSON responses, successful or not,
// carry the server's protocol, as gRPC responses do.
func TestResponsesCarryProtocol(t *testing.T) {
	ts, cleanup := newTestServer(t)
	defer cleanup()
	for name, body := range map[string]string{
		"success": `{"claimantId": "test", "inserts": [{"queue": "/q", "value": "x"}]}`,
		"error":   `{"claimantId": "test", "deletes": [{"id": "none", "version": 1, "queue": "/q"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := http.Post(ts.URL+"/api/v0/modify", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			resp.Body.Close()
			if got, want := resp.Header.Get(version.ProtocolHeader), version.FormatProtocols(version.ServedProtocols); got != want {
				t.Errorf("status %d: protocol header %q, want %q", resp.StatusCode, got, want)
			}
		})
	}
}

// TestRequestsDeclareProtocol checks that a JSON request is read under the
// protocol its header declares: none is protocol 1, which has no change
// modes, and one the server does not serve is refused.
func TestRequestsDeclareProtocol(t *testing.T) {
	ts, cleanup := newTestServer(t)
	defer cleanup()
	lease := `{"claimantId": "test", "changes": [{"oldId": {"id": "t", "version": 1, "queue": "/q"}, "newData": {"atMs": "1"}, "mode": "CHANGE_LEASE"}]}`
	for _, tc := range []struct {
		name, protocol string
		want           int
	}{
		{"a lease with no protocol", "", http.StatusBadRequest},
		{"a lease at protocol 1", "1", http.StatusBadRequest},
		{"an unserved protocol", strconv.Itoa(version.Protocol + 1), http.StatusNotImplemented},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v0/modify", strings.NewReader(lease))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			if tc.protocol != "" {
				req.Header.Set(version.ProtocolHeader, tc.protocol)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// TestUnknownJSONFieldsRefused checks that a JSON request with a field the
// server does not know is refused on both routes, the REST one and the
// connect one, and nothing is done.
func TestUnknownJSONFieldsRefused(t *testing.T) {
	ts, cleanup := newTestServer(t)
	defer cleanup()
	for _, tc := range []struct{ name, path, body string }{
		{"REST, nested", "/api/v0/modify", `{"claimantId": "test", "inserts": [{"queue": "/strict", "bogus": 1}]}`},
		{"REST, top level", "/api/v0/modify", `{"claimantId": "test", "inserts": [{"queue": "/strict"}], "bogus": true}`},
		{"connect, nested", "/api.EntroQ/Modify", `{"claimantId": "test", "inserts": [{"queue": "/strict", "bogus": 1}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(ts.URL+tc.path, "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "bogus") {
				t.Errorf("Unknown field: want 400 naming it, got %d %s", resp.StatusCode, body)
			}
		})
	}
	resp, err := http.Get(ts.URL + "/api/v0/tasks?queue=/strict")
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), `"queue":"/strict"`) {
		t.Errorf("After the refused requests: want nothing inserted, got %s", b)
	}
}

// TestStreamTasksOverConnect covers the streaming route, which nothing else does.
//
// streamAdapter satisfies the gRPC service's stream interface by EMBEDDING it and
// leaving it nil, so every method the service calls that the adapter does not
// define itself is a nil dereference. QSvc.StreamTasks opens by setting the
// protocol header on the stream, which is such a method -- so this route panicked
// on its first statement and dropped the connection. The gRPC-side test fake was
// given that method; the production adapter was not, and nothing covered it.
func TestStreamTasksOverConnect(t *testing.T) {
	ts, cleanup := newTestServer(t)
	defer cleanup()

	if code, _ := postJSON(t, ts.URL+"/api/v0/modify", map[string]any{
		"claimantId": "test",
		"inserts":    []any{map[string]any{"queue": "/stream/q", "value": "x"}},
	}); code != http.StatusOK {
		t.Fatalf("seed insert: status %d", code)
	}

	client := apiconnect.NewEntroQClient(http.DefaultClient, ts.URL)
	stream, err := client.StreamTasks(context.Background(),
		connect.NewRequest(&pb.TasksRequest{Queue: "/stream/q"}))
	if err != nil {
		t.Fatalf("StreamTasks: %v", err)
	}
	defer stream.Close()

	var queues []string
	for stream.Receive() {
		for _, task := range stream.Msg().GetTasks() {
			queues = append(queues, task.GetQueue())
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("receiving: %v", err)
	}
	if len(queues) == 0 {
		t.Fatal("stream delivered no tasks")
	}
	for _, q := range queues {
		if q != "/stream/q" {
			t.Errorf("streamed a task from %q, want /stream/q", q)
		}
	}
}
