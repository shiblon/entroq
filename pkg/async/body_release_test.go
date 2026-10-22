package async_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shiblon/entroq/pkg/async"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// TestSidecarResponseEndsWhileRequestBodyIsOpen is an upstream that answers in
// full without reading the whole request, to a client that keeps its request
// body open. The exchange is over once the response is, so the sender must
// stop reading the body and finish the response rather than wait for a client
// that is itself waiting for the response to end.
func TestSidecarResponseEndsWhileRequestBodyIsOpen(t *testing.T) {
	dialer := &net.Dialer{}
	for name, client := range map[string]*http.Client{
		"http1": {},
		"h2c": {Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
				return dialer.DialContext(ctx, network, address)
			},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			// h2c serves HTTP/1 as well, and the receiver forwards each request
			// in the protocol it arrived in. Over HTTP/1 the upstream must be
			// full duplex, or net/http drains the request body before it sends
			// the response, and the upstream itself waits on the open body.
			upstream := httptest.NewServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := http.NewResponseController(w).EnableFullDuplex(); err != nil && !errors.Is(err, http.ErrNotSupported) {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				first := make([]byte, len("hello"))
				if _, err := io.ReadFull(r.Body, first); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				io.WriteString(w, "done")
			}), &http2.Server{}))
			defer upstream.Close()

			eq, stopEQ := mustStartEntroQ(ctx, t, eqmem.Opener())
			defer stopEQ()
			stopReceiver := mustStartReceivers(ctx, t, eq, "/open/svc", upstream.URL, 1)
			defer stopReceiver()

			sender := async.NewSender(eq, "",
				async.WithSenderDomainSuffix(".test"),
				async.WithSenderNamespace("open"),
				async.WithSenderRequestTimeout(10*time.Second),
			)
			listener, err := net.Listen("tcp", "localhost:0")
			if err != nil {
				t.Fatalf("listen for sender: %v", err)
			}
			senderCtx, stopSender := context.WithCancel(ctx)
			senderDone := make(chan error, 1)
			go func() { senderDone <- sender.Serve(senderCtx, listener) }()
			defer func() {
				stopSender()
				<-senderDone
			}()

			requestReader, requestWriter := io.Pipe()
			defer requestWriter.Close()
			go requestWriter.Write([]byte("hello"))
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+listener.Addr().String()+"/open", requestReader)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Host = "svc.test"

			type result struct {
				status int
				body   string
				err    error
			}
			got := make(chan result, 1)
			go func() {
				response, err := client.Do(req)
				if err != nil {
					got <- result{err: err}
					return
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				got <- result{status: response.StatusCode, body: string(body), err: err}
			}()
			select {
			case r := <-got:
				if r.err != nil {
					t.Fatalf("response: %v", r.err)
				}
				if r.status != http.StatusOK || r.body != "done" {
					t.Errorf("response = %d %q, want %d %q", r.status, r.body, http.StatusOK, "done")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the response did not end while the request body stayed open")
			}
		})
	}
}
