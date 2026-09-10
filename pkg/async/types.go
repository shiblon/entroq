package async

import (
	"net/http"
)

// hopByHop lists headers that must not be forwarded by a proxy.
// Content-Length is included because the proxy recalculates it from the
// actual body written; forwarding the upstream value causes mismatches when
// the body passes through JSON encoding.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Content-Length":      true,
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

// FrameControl carries the state shared by request and response frames.
// ReplyQueue is the full queue name on which the frame sender awaits the next
// frame. It is required for non-final frames and omitted from final frames,
// which do not receive an acknowledgement. A changed ReplyQueue requests a
// queue switch; the queue name and its policy components remain opaque to the
// peer. A final Envelope is a best-effort cancellation and carries Error; a
// final Response may represent ordinary EOF and therefore need not carry one.
type FrameControl struct {
	Session    string `json:"session"`
	ReplyQueue string `json:"reply_queue,omitempty"`
	Final      bool   `json:"final,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Envelope is a request-direction frame used for sidecar-to-sidecar
// communication. The initial envelope carries the HTTP request metadata;
// continuation frames may omit fields that have not changed.
type Envelope struct {
	FrameControl

	Method  string      `json:"method,omitempty"`
	Path    string      `json:"path,omitempty"`
	Headers http.Header `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
}

// Response is a response-direction frame. The initial response carries HTTP
// response metadata; continuation frames may omit fields that have not
// changed. When the upstream is unreachable or the sidecar encounters an
// infrastructure error, StatusCode is set to an appropriate HTTP gateway code
// (502 Bad Gateway, 504 Gateway Timeout, etc.) and Error carries the internal
// detail.
type Response struct {
	FrameControl

	StatusCode int         `json:"status_code,omitempty"`
	Headers    http.Header `json:"headers,omitempty"`
	Body       []byte      `json:"body,omitempty"`
}
