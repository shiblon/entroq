package async_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/async"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/eqsvcgrpc"
)

// TestServiceGCWithSidecarGCOff validates the target topology once eqlink's
// local GC is disabled: no async.RunGCLoop runs anywhere (mirroring
// `eqlink run --run-gc=false`), and the service collects garbage on its own. It
// confirms both that a live request is unharmed by the service GC scanning
// underneath it, and that an orphaned response queue is reaped by the service.
func TestServiceGCWithSidecarGCOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	upstream := startEchoUpstream()
	defer upstream.Close()

	// Service-side GC on, scanning aggressively; deliberately no RunGCLoop.
	eq, stopEQ := mustStartEntroQ(ctx, t, eqmem.Opener(),
		eqsvcgrpc.WithGC(), eqsvcgrpc.WithGCInterval(20*time.Millisecond))
	defer stopEQ()

	const (
		namespace = "payments"
		prefix    = "/" + namespace + "/svc"
	)
	stopSvc := mustStartReceivers(ctx, t, eq, prefix, upstream.URL, 2)
	defer stopSvc()

	sender := async.NewSender(eq, "",
		async.WithSenderDomainSuffix(".test"),
		async.WithSenderNamespace(namespace),
	)

	// (a) A live request succeeds even though service GC scans every 20ms: the
	// sender stamps its response queue collectable far in the future (timeout +
	// grace), so GC sees it as not-yet-due and leaves it alone.
	req := httptest.NewRequest(http.MethodGet, "http://svc.test/live", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	sender.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("live request failed with service GC running: status %d: %s", w.Code, w.Body)
	}

	// (b) An orphaned response queue -- a response the sender abandoned, whose
	// collect-at time has already passed -- is reaped by the service GC.
	past := time.Now().Add(-time.Hour).Unix()
	orphan := fmt.Sprintf("%s/response/gc=%d/deadbeef", prefix, past)
	if _, err := eq.Modify(ctx, entroq.InsertingInto(orphan, entroq.WithRawValue([]byte("{}")))); err != nil {
		t.Fatalf("seed orphan response queue: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		sizes, err := eq.Queues(ctx)
		if err != nil {
			t.Fatalf("Queues: %v", err)
		}
		if sizes[orphan] == 0 {
			break // collected by the service GC
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("orphan queue %q was not collected by the service GC (size %d)", orphan, sizes[orphan])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
