package workgateway

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// TestWS_OK proves the identical Bridge works over WebSocket: a worker dials in,
// receives the work message, replies ok, and the input task is committed. Same
// core as TestBridge_OK, different transport.
func TestWS_OK(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	defer eq.Close()

	const q = "in"
	if _, err := eq.Modify(ctx, entroq.InsertingInto(q, entroq.WithValue("hello"))); err != nil {
		t.Fatalf("insert: %v", err)
	}

	srvCtx, srvCancel := context.WithCancel(ctx)
	defer srvCancel()
	srv := httptest.NewServer(Handler(srvCtx, eq, 30*time.Second))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/work?queue=" + q
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	// Play the worker: read the work message, reply ok.
	var wm struct {
		Type string      `json:"type"`
		Task entroq.Task `json:"task"`
	}
	if err := wsjson.Read(ctx, c, &wm); err != nil {
		t.Fatalf("read work: %v", err)
	}
	if wm.Type != "work" {
		t.Errorf("message type = %q, want %q", wm.Type, "work")
	}
	if got := string(wm.Task.Value); got != `"hello"` {
		t.Errorf("task value = %s, want %q", got, `"hello"`)
	}
	if err := wsjson.Write(ctx, c, map[string]string{"type": "result", "outcome": "ok"}); err != nil {
		t.Fatalf("write result: %v", err)
	}

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact(q)); err != nil {
		t.Fatalf("wait queue empty: %v", err)
	}
	c.Close(websocket.StatusNormalClosure, "")
	srvCancel()
}
