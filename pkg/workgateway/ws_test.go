package workgateway

import (
	"context"
	"encoding/json"
	"net/http"
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

	gw := NewServer(eq, 30*time.Second, defaultEntroQTimeout)
	defer gw.Close()
	srv := httptest.NewServer(gw)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/work?queue=in&work=1"
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	readHello(t, ctx, c)

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
	gw := NewServer(eq, lease, defaultEntroQTimeout)
	defer gw.Close()
	srv := httptest.NewServer(gw)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/work?queue=in&work=1"
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	readHello(t, ctx, c)

	var dw doWorkMsg
	if err := wsjson.Read(ctx, c, &dw); err != nil {
		t.Fatalf("read doWork: %v", err)
	}
	if dw.Type != msgDoWork {
		t.Fatalf("got %q, want %q", dw.Type, msgDoWork)
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

// readHello reads the gateway's opening hello on a WebSocket, as every client
// does before the first phase message.
func readHello(t *testing.T, ctx context.Context, c *websocket.Conn) {
	t.Helper()
	var h helloMsg
	if err := wsjson.Read(ctx, c, &h); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if h.Type != msgHello || h.Protocol != Protocol {
		t.Fatalf("hello: got %+v, want protocol %d", h, Protocol)
	}
}

// TestWS_ShutdownDrains: Server.Shutdown lets each connection finish its task
// and closes it normally, and refuses new connections meanwhile.
func TestWS_ShutdownDrains(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	gw := NewServer(eq, time.Minute, defaultEntroQTimeout)
	defer gw.Close()
	srv := httptest.NewServer(gw)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/work?queue=in&work=1"
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	readHello(t, ctx, c)
	var dw doWorkMsg
	if err := wsjson.Read(ctx, c, &dw); err != nil {
		t.Fatalf("read doWork: %v", err)
	}

	shutErr := make(chan error, 1)
	go func() { shutErr <- gw.Shutdown(ctx) }()
	time.Sleep(20 * time.Millisecond) // let the drain begin

	if _, resp, err := websocket.Dial(ctx, wsURL, nil); err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("dial while draining: got %v (resp %v), want a 503 refusal", err, resp)
	}

	del := &pb.ModifyRequest{Deletes: []*pb.TaskID{{Id: dw.Task.Id, Version: dw.Task.Version, Queue: dw.Task.Queue}}}
	if err := wsjson.Write(ctx, c, result{Type: msgResult, disposition: disposition{Outcome: outcomeOK}, Modification: &wireModReq{del}}); err != nil {
		t.Fatalf("write result: %v", err)
	}
	if err := <-shutErr; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	var msg json.RawMessage
	if err := wsjson.Read(ctx, c, &msg); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("after the drain: got %v, want a normal close", err)
	}
	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("the drained task must commit: %v", err)
	}
}
