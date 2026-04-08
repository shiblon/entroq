package async

import "encoding/json"

// Envelope is the task payload used for sidecar-to-sidecar communication.
// It carries everything needed to reconstruct an HTTP request on the receiving
// end, plus the response queue name for the reply.
type Envelope struct {
	Method        string            `json:"method"`
	Path          string            `json:"path"`
	Headers       map[string]string `json:"headers,omitempty"`
	Body          json.RawMessage   `json:"body,omitempty"`
	ResponseQueue string            `json:"response_queue"`
}

// Response is the task payload enqueued onto the response queue after the
// upstream service handles a request. When the upstream is unreachable or the
// sidecar encounters an infrastructure error, StatusCode is set to an
// appropriate HTTP gateway code (502 Bad Gateway, 504 Gateway Timeout, etc.)
// and Error carries the internal detail. The sender always reconstructs its
// HTTP response from StatusCode and Body alone.
type Response struct {
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       json.RawMessage   `json:"body,omitempty"`
	Error      string            `json:"error,omitempty"`
}
