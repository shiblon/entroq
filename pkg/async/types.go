package async

import (
	"encoding/json"
	"net/http"
)

// hopByHop lists headers that are meaningful only for a single TCP hop and
// must not be forwarded by a proxy to the next hop.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// copyHeaders copies src into a new http.Header, dropping hop-by-hop headers.
// All values for each remaining header are preserved.
func copyHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for k, vs := range src {
		if !hopByHop[k] {
			dst[k] = vs
		}
	}
	return dst
}

// Envelope is the task payload used for sidecar-to-sidecar communication.
// It carries everything needed to reconstruct an HTTP request on the receiving
// end, plus the response queue name for the reply.
type Envelope struct {
	Method        string          `json:"method"`
	Path          string          `json:"path"`
	Headers       http.Header     `json:"headers,omitempty"`
	Body          json.RawMessage `json:"body,omitempty"`
	ResponseQueue string          `json:"response_queue"`
}

// Response is the task payload enqueued onto the response queue after the
// upstream service handles a request. When the upstream is unreachable or the
// sidecar encounters an infrastructure error, StatusCode is set to an
// appropriate HTTP gateway code (502 Bad Gateway, 504 Gateway Timeout, etc.)
// and Error carries the internal detail. The sender always reconstructs its
// HTTP response from StatusCode and Body alone.
type Response struct {
	StatusCode int             `json:"status_code"`
	Headers    http.Header     `json:"headers,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
	Error      string          `json:"error,omitempty"`
}
