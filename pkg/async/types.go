package async

import (
	"net/http"
	"sort"
	"strings"
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
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// copyHeaders copies src into a new http.Header, dropping hop-by-hop headers.
// All values for each remaining header are preserved.
func copyHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for k, vs := range src {
		canonical := http.CanonicalHeaderKey(k)
		// HTTP/2 permits exactly one TE value, "trailers". gRPC requires it,
		// so preserve that value while still dropping every other hop-by-hop
		// transfer-coding request.
		if canonical == "Te" {
			for _, value := range vs {
				if strings.EqualFold(strings.TrimSpace(value), "trailers") {
					dst["Te"] = []string{"trailers"}
				}
			}
			continue
		}
		if !hopByHop[canonical] {
			dst[canonical] = append([]string(nil), vs...)
		}
	}
	return dst
}

func trailerKeys(h http.Header) []string {
	keys := make([]string, 0, len(h))
	for key := range h {
		keys = append(keys, http.CanonicalHeaderKey(key))
	}
	sort.Strings(keys)
	return keys
}

func headerWithKeys(keys []string) http.Header {
	h := make(http.Header, len(keys))
	for _, key := range keys {
		h[http.CanonicalHeaderKey(key)] = nil
	}
	return h
}

// FrameControl carries the state shared by request and response frames.
// ReplyQueue is the full queue name on which the frame sender awaits the next
// frame in this lane. Data and empty acknowledgement frames alternate within
// one lane, so a changed ReplyQueue requests a lane-local queue switch. Queue
// names and their policy components remain opaque to the peer.
//
// Final closes the frame's direction and therefore omits ReplyQueue. A final
// Envelope half-closes the request body; a final Response completes the HTTP
// exchange. A terminal acknowledgement is permitted only when it carries an
// error; benign terminal data frames deliberately receive no final ACK.
type FrameControl struct {
	Session    string `json:"session"`
	ReplyQueue string `json:"reply_queue,omitempty"`
	Final      bool   `json:"final,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Envelope is a request-direction data frame. The initial envelope carries
// HTTP request metadata and ResponseQueue, the full queue on which the sender
// accepts response data. Continuation frames carry arbitrary request body
// bytes. The terminal frame carries request trailers and half-closes the body.
type Envelope struct {
	FrameControl

	ResponseQueue string      `json:"response_queue,omitempty"`
	Method        string      `json:"method,omitempty"`
	Path          string      `json:"path,omitempty"`
	ProtocolMajor int         `json:"protocol_major,omitempty"`
	ContentLength int64       `json:"content_length,omitempty"`
	Headers       http.Header `json:"headers,omitempty"`
	TrailerKeys   []string    `json:"trailer_keys,omitempty"`
	Body          []byte      `json:"body,omitempty"`
	Trailers      http.Header `json:"trailers,omitempty"`
}

// Response is a response-direction data frame. The initial response carries
// HTTP response metadata; continuation frames carry arbitrary response body
// bytes. The terminal frame carries response trailers. When the upstream is
// unreachable or EQLink encounters an infrastructure error, StatusCode is set
// to an appropriate HTTP gateway code and Error carries the internal detail.
type Response struct {
	FrameControl

	StatusCode  int         `json:"status_code,omitempty"`
	Headers     http.Header `json:"headers,omitempty"`
	TrailerKeys []string    `json:"trailer_keys,omitempty"`
	Body        []byte      `json:"body,omitempty"`
	Trailers    http.Header `json:"trailers,omitempty"`
}
