package eqmr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	httpObjectStoreProtocol        = "entroq.eqmr.object-store/1"
	httpObjectStoreInfoPath        = "/v1/info"
	httpObjectStoreOpenPath        = "/v1/open"
	httpObjectStoreDeletePath      = "/v1/delete"
	httpObjectStoreObjectPath      = "/v1/objects/"
	httpObjectStoreMaxControlBytes = 64 << 10
)

type httpObjectStoreInfo struct {
	Protocol string `json:"protocol"`
	Driver   string `json:"driver"`
	Identity string `json:"identity"`
}

type httpObjectStore struct {
	client  *http.Client
	baseURL *url.URL
}

func newHTTPObjectStore(client *http.Client, baseURL string) (*httpObjectStore, error) {
	if client == nil {
		return nil, fmt.Errorf("eqmr object store: nil HTTP client")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("eqmr object store: parse base URL: %w", err)
	}
	if !u.IsAbs() || u.Host == "" {
		return nil, fmt.Errorf("eqmr object store: base URL must be absolute")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("eqmr object store: base URL must not contain a query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return &httpObjectStore{client: client, baseURL: u}, nil
}

func (s *httpObjectStore) info(ctx context.Context) (httpObjectStoreInfo, error) {
	var info httpObjectStoreInfo
	req, err := s.request(ctx, http.MethodGet, httpObjectStoreInfoPath, nil)
	if err != nil {
		return info, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return info, fmt.Errorf("eqmr object store info: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info, httpObjectStoreResponseError("info", resp)
	}
	if err := decodeHTTPObjectStoreJSON(resp.Body, &info); err != nil {
		return info, fmt.Errorf("eqmr object store info: %w", err)
	}
	if info.Protocol != httpObjectStoreProtocol {
		return info, fmt.Errorf("eqmr object store info: protocol is %q, want %q", info.Protocol, httpObjectStoreProtocol)
	}
	if info.Driver == "" || info.Identity == "" {
		return info, fmt.Errorf("eqmr object store info: driver and identity are required")
	}
	return info, nil
}

func (s *httpObjectStore) put(ctx context.Context, objectID string, body io.Reader) (json.RawMessage, error) {
	if objectID == "" {
		return nil, fmt.Errorf("eqmr object store put: object ID is required")
	}
	if body == nil {
		return nil, fmt.Errorf("eqmr object store put %q: nil body", objectID)
	}
	req, err := s.request(ctx, http.MethodPut, httpObjectStoreObjectPath+url.PathEscape(objectID), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("eqmr object store put %q: %w", objectID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, httpObjectStoreResponseError("put "+objectID, resp)
	}
	var out struct {
		Ref json.RawMessage `json:"ref"`
	}
	if err := decodeHTTPObjectStoreJSON(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("eqmr object store put %q: %w", objectID, err)
	}
	if len(out.Ref) == 0 || bytes.Equal(bytes.TrimSpace(out.Ref), []byte("null")) {
		return nil, fmt.Errorf("eqmr object store put %q: response ref is required", objectID)
	}
	return append(json.RawMessage(nil), out.Ref...), nil
}

func (s *httpObjectStore) open(ctx context.Context, ref json.RawMessage) (io.ReadCloser, error) {
	resp, err := s.refRequest(ctx, "open", httpObjectStoreOpenPath, ref)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, httpObjectStoreResponseError("open", resp)
	}
	return resp.Body, nil
}

func (s *httpObjectStore) delete(ctx context.Context, ref json.RawMessage) error {
	resp, err := s.refRequest(ctx, "delete", httpObjectStoreDeletePath, ref)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return httpObjectStoreResponseError("delete", resp)
	}
	return nil
}

func (s *httpObjectStore) refRequest(ctx context.Context, operation, path string, ref json.RawMessage) (*http.Response, error) {
	if len(ref) == 0 || !json.Valid(ref) || bytes.Equal(bytes.TrimSpace(ref), []byte("null")) {
		return nil, fmt.Errorf("eqmr object store %s: ref must be non-null JSON", operation)
	}
	body, err := json.Marshal(struct {
		Ref json.RawMessage `json:"ref"`
	}{Ref: ref})
	if err != nil {
		return nil, fmt.Errorf("eqmr object store %s: encode ref: %w", operation, err)
	}
	req, err := s.request(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("eqmr object store %s: %w", operation, err)
	}
	return resp, nil
}

func (s *httpObjectStore) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	u := *s.baseURL
	u.Path = strings.TrimRight(s.baseURL.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("eqmr object store %s %s: %w", method, path, err)
	}
	return req, nil
}

type httpObjectStoreError struct {
	Operation  string
	StatusCode int
	Code       string
	Message    string
}

func (e *httpObjectStoreError) Error() string {
	detail := e.Message
	if detail == "" {
		detail = http.StatusText(e.StatusCode)
	}
	if e.Code != "" {
		detail = e.Code + ": " + detail
	}
	return fmt.Sprintf("eqmr object store %s: HTTP %d: %s", e.Operation, e.StatusCode, detail)
}

func (e *httpObjectStoreError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

func httpObjectStoreResponseError(operation string, resp *http.Response) error {
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	err := decodeHTTPObjectStoreJSON(resp.Body, &body)
	if err != nil && !errors.Is(err, io.EOF) {
		body.Message = "invalid error response: " + err.Error()
	}
	return &httpObjectStoreError{
		Operation:  operation,
		StatusCode: resp.StatusCode,
		Code:       body.Code,
		Message:    body.Message,
	}
}

func decodeHTTPObjectStoreJSON(r io.Reader, dst any) error {
	limited := io.LimitReader(r, httpObjectStoreMaxControlBytes+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if len(b) > httpObjectStoreMaxControlBytes {
		return fmt.Errorf("control response exceeds %d bytes", httpObjectStoreMaxControlBytes)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return io.EOF
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}
