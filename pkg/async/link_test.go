package async_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shiblon/entroq/pkg/async"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// TestSharedEQSidecar verifies the basic sidecar pattern: sender and receiver
// share one EQ instance. A request inserted by the sender arrives at the
// receiver and a response comes back. This is the single-datacenter (or
// WAN-accessible shared EQ) scenario.
func TestSharedEQSidecar(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	upstream := startUpstream()
	defer upstream.Close()

	eq, stopEQ := mustStartEntroQ(ctx, t, eqmem.Opener())
	defer stopEQ()

	const queue = "svc-test"
	stopReceivers := mustStartReceivers(ctx, t, eq, "/"+queue, upstream.URL, 2)
	defer stopReceivers()

	sender := async.NewSender(eq, "", async.WithSenderDomainSuffix(".test"))
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://%s.test/ping", queue), nil).WithContext(ctx)
	w := httptest.NewRecorder()
	sender.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200 (body: %s)", w.Code, w.Body)
	}
}
