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
	pb "github.com/shiblon/entroq/api"
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
	srv := httptest.NewServer(Handler(srvCtx, eq, 30*time.Second, defaultEntroQTimeout))
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
	if got := dw.Task.Value.GetStringValue(); got != "hello" {
		t.Errorf("task value = %q, want %q", got, "hello")
	}
	del := &pb.ModifyRequest{Deletes: []*pb.TaskID{{Id: dw.Task.Id, Version: dw.Task.Version, Queue: dw.Task.Queue}}}
	if err := wsjson.Write(ctx, c, result{
		Type:         msgResult,
		disposition:  disposition{Outcome: outcomeOK},
		Modification: &wireModReq{del},
	}); err != nil {
		t.Fatalf("write result: %v", err)
	}

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("wait queue empty: %v", err)
	}
	c.Close(websocket.StatusNormalClosure, "")
	srvCancel()
}

// TestWS_ClientDropReclaims is the WebSocket analog of TestBridge_ClientDropReclaims:
// a worker that dials in, receives a task, and abruptly drops the connection
// leaves the (uncommitted) task reclaimable once the lease expires.
func TestWS_ClientDropReclaims(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	lease := 200 * time.Millisecond
	srvCtx, srvCancel := context.WithCancel(ctx)
	defer srvCancel()
	srv := httptest.NewServer(Handler(srvCtx, eq, lease, defaultEntroQTimeout))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/work?queue=in&work=1"
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	var dw doWorkMsg
	if err := wsjson.Read(ctx, c, &dw); err != nil {
		t.Fatalf("read doWork: %v", err)
	}
	c.CloseNow() // abruptly drop without replying

	// The task must be reclaimable once the lease lapses (at-least-once over WS).
	deadline := time.After(5 * time.Second)
	for {
		claimed, err := eq.TryClaim(ctx, entroq.From("in"), entroq.ClaimFor(time.Second))
		if err != nil {
			t.Fatalf("try claim: %v", err)
		}
		if claimed != nil {
			return
		}
		select {
		case <-deadline:
			t.Fatal("task never reclaimable after a WS drop + lease expiry")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
