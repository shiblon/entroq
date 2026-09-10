package async

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"

	"golang.org/x/net/http2"
)

// protocolTransport keeps the local caller's HTTP major version when opening
// the receiver-to-service hop. In particular, a cleartext HTTP/2 request uses
// prior-knowledge h2c rather than silently degrading a gRPC stream to HTTP/1.1.
type protocolTransport struct {
	http1    *http.Transport
	http2TLS *http2.Transport
	h2c      *http2.Transport
}

func newProtocolHTTPClient(tlsConfig *tls.Config) *http.Client {
	cloneTLS := func() *tls.Config {
		if tlsConfig == nil {
			return nil
		}
		return tlsConfig.Clone()
	}
	dialer := &net.Dialer{}
	return &http.Client{Transport: &protocolTransport{
		http1: &http.Transport{
			TLSClientConfig:     cloneTLS(),
			MaxIdleConnsPerHost: 32,
			// A protocol-major-one request must remain HTTP/1.1 even when the
			// TLS peer also advertises HTTP/2.
			TLSNextProto: make(map[string]func(string, *tls.Conn) http.RoundTripper),
		},
		http2TLS: &http2.Transport{TLSClientConfig: cloneTLS()},
		h2c: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
				return dialer.DialContext(ctx, network, address)
			},
		},
	}}
}

func (t *protocolTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.ProtoMajor < 2 {
		return t.http1.RoundTrip(request)
	}
	if request.URL.Scheme == "http" {
		return t.h2c.RoundTrip(request)
	}
	return t.http2TLS.RoundTrip(request)
}

func (t *protocolTransport) CloseIdleConnections() {
	t.http1.CloseIdleConnections()
	t.http2TLS.CloseIdleConnections()
	t.h2c.CloseIdleConnections()
}
