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
)

// TestWS_OK proves the identical Bridge works over WebSocket: a worker dials in,
// registers, receives the task, replies ok with a delete, and the input drains.
// Same core as TestBridge_OKDeletes, different transport.
func TestWS_OK(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	srvCtx, srvCancel := context.WithCancel(ctx)
	defer srvCancel()
	srv := httptest.NewServer(Handler(srvCtx, eq, 30*time.Second))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/work?queue=in&work=1"
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	var dw doWorkMsg
	if err := wsjson.Read(ctx, c, &dw); err != nil {
		t.Fatalf("read doWork: %v", err)
	}
	if dw.Type != msgDoWork {
		t.Errorf("got %q, want %q", dw.Type, msgDoWork)
	}
	if got := string(dw.Task.Value); got != `"hello"` {
		t.Errorf("task value = %s, want %q", got, `"hello"`)
	}
	if err := wsjson.Write(ctx, c, result{
		Type:         msgResult,
		Outcome:      outcomeOK,
		Modification: &modification{Deletes: []taskRef{{ID: dw.Task.ID, Version: dw.Task.Version}}},
	}); err != nil {
		t.Fatalf("write result: %v", err)
	}

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("wait queue empty: %v", err)
	}
	c.Close(websocket.StatusNormalClosure, "")
	srvCancel()
}
