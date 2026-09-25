package eqmr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestHTTPObjectStoreLifecycle(t *testing.T) {
	t.Parallel()
	driver := newTestHTTPObjectDriver(t)
	server := httptest.NewServer(driver)
	t.Cleanup(server.Close)

	store, err := newHTTPObjectStore(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ctx := context.Background()
	info, err := store.info(ctx)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Driver != testHTTPObjectDriverName || info.Identity != testHTTPObjectIdentity {
		t.Fatalf("info = %#v", info)
	}

	content := "a streamed immutable run"
	ref, err := store.put(ctx, "run-000001", struct{ io.Reader }{strings.NewReader(content)})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	wantRef := json.RawMessage(`{"object_id":"run-000001"}`)
	if !bytes.Equal(ref, wantRef) {
		t.Fatalf("ref = %s, want %s", ref, wantRef)
	}
	if !driver.sawChunked() {
		t.Fatal("PUT did not use HTTP chunked transfer for an unknown-length body")
	}

	// Repeating the same PUT is an idempotent success and returns the same ref.
	retryRef, err := store.put(ctx, "run-000001", strings.NewReader(content))
	if err != nil {
		t.Fatalf("repeat put: %v", err)
	}
	if !bytes.Equal(retryRef, ref) {
		t.Fatalf("repeat ref = %s, want %s", retryRef, ref)
	}

	r, err := store.open(ctx, ref)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, readErr := io.ReadAll(r)
	closeErr := r.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != content {
		t.Fatalf("content = %q, want %q", got, content)
	}

	if err := store.delete(ctx, ref); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.delete(ctx, ref); err != nil {
		t.Fatalf("repeat delete: %v", err)
	}
	if _, err := store.open(ctx, ref); err == nil {
		t.Fatal("open deleted object succeeded")
	} else {
		var httpErr *httpObjectStoreError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusNotFound || httpErr.Retryable() {
			t.Fatalf("open deleted object: %v", err)
		}
	}
}

func TestHTTPObjectStoreRejectsConflictingPut(t *testing.T) {
	t.Parallel()
	driver := newTestHTTPObjectDriver(t)
	server := httptest.NewServer(driver)
	t.Cleanup(server.Close)
	store, err := newHTTPObjectStore(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	if _, err := store.put(context.Background(), "same-id", strings.NewReader("first")); err != nil {
		t.Fatalf("first put: %v", err)
	}
	_, err = store.put(context.Background(), "same-id", strings.NewReader("different"))
	var httpErr *httpObjectStoreError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusConflict || httpErr.Retryable() {
		t.Fatalf("conflicting put error = %v", err)
	}
}

func TestHTTPObjectStoreRequiresMatchingProtocol(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, w, http.StatusOK, httpObjectStoreInfo{
			Protocol: "entroq.eqmr.object-store/99",
			Driver:   testHTTPObjectDriverName,
			Identity: testHTTPObjectIdentity,
		})
	}))
	t.Cleanup(server.Close)
	store, err := newHTTPObjectStore(server.Client(), server.URL)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := store.info(context.Background()); err == nil || !strings.Contains(err.Error(), "want \""+httpObjectStoreProtocol+"\"") {
		t.Fatalf("info error = %v", err)
	}
}

const (
	testHTTPObjectDriverName = "example.test/object-store/v1"
	testHTTPObjectIdentity   = "sha256:test-store"
)

type testHTTPObjectDriver struct {
	t             *testing.T
	mu            sync.Mutex
	objects       map[string][]byte
	sawChunkedPut bool
}

func newTestHTTPObjectDriver(t *testing.T) *testHTTPObjectDriver {
	return &testHTTPObjectDriver{t: t, objects: make(map[string][]byte)}
}

func (d *testHTTPObjectDriver) sawChunked() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sawChunkedPut
}

func (d *testHTTPObjectDriver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == httpObjectStoreInfoPath:
		writeTestJSON(d.t, w, http.StatusOK, httpObjectStoreInfo{
			Protocol: httpObjectStoreProtocol,
			Driver:   testHTTPObjectDriverName,
			Identity: testHTTPObjectIdentity,
		})

	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, httpObjectStoreObjectPath):
		d.put(w, r)

	case r.Method == http.MethodPost && r.URL.Path == httpObjectStoreOpenPath:
		d.open(w, r)

	case r.Method == http.MethodPost && r.URL.Path == httpObjectStoreDeletePath:
		d.delete(w, r)

	default:
		writeTestJSON(d.t, w, http.StatusNotFound, map[string]string{"code": "not_found", "message": "unknown operation"})
	}
}

func (d *testHTTPObjectDriver) put(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, httpObjectStoreObjectPath)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		d.t.Errorf("read PUT: %v", err)
		writeTestJSON(d.t, w, http.StatusBadRequest, map[string]string{"code": "read", "message": err.Error()})
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(r.TransferEncoding) == 1 && r.TransferEncoding[0] == "chunked" {
		d.sawChunkedPut = true
	}
	if old, ok := d.objects[id]; ok {
		if !bytes.Equal(old, b) {
			writeTestJSON(d.t, w, http.StatusConflict, map[string]string{"code": "object_conflict", "message": "object ID already has different contents"})
			return
		}
		writeTestJSON(d.t, w, http.StatusOK, map[string]any{"ref": map[string]string{"object_id": id}})
		return
	}
	d.objects[id] = append([]byte(nil), b...)
	writeTestJSON(d.t, w, http.StatusCreated, map[string]any{"ref": map[string]string{"object_id": id}})
}

func (d *testHTTPObjectDriver) open(w http.ResponseWriter, r *http.Request) {
	id, ok := d.requestObjectID(w, r)
	if !ok {
		return
	}
	d.mu.Lock()
	b, found := d.objects[id]
	d.mu.Unlock()
	if !found {
		writeTestJSON(d.t, w, http.StatusNotFound, map[string]string{"code": "object_not_found", "message": "object does not exist"})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(b); err != nil {
		d.t.Errorf("write OPEN: %v", err)
	}
}

func (d *testHTTPObjectDriver) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := d.requestObjectID(w, r)
	if !ok {
		return
	}
	d.mu.Lock()
	delete(d.objects, id)
	d.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (d *testHTTPObjectDriver) requestObjectID(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		Ref struct {
			ObjectID string `json:"object_id"`
		} `json:"ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Ref.ObjectID == "" {
		writeTestJSON(d.t, w, http.StatusUnprocessableEntity, map[string]string{"code": "invalid_ref", "message": "ref requires object_id"})
		return "", false
	}
	return req.Ref.ObjectID, true
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
