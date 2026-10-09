package workgateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
)

// readFixture is a gateway over a real backend with two tasks and two docs in
// it. The reads are testable without a conn, because a read is answered from
// the client and handed straight back: nothing about it involves a turn.
func readFixture(ctx context.Context, t *testing.T) *Gateway {
	t.Helper()
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	if _, err := client.Modify(ctx,
		entroq.InsertingInto("inbox", entroq.WithValue([]byte(`{"n":1}`))),
		entroq.InsertingInto("inbox", entroq.WithValue([]byte(`{"n":2}`))),
		entroq.InsertingInto("other"),
		entroq.PuttingDocInto("cfg", entroq.WithKeys("limits", ""), entroq.WithRawContent(json.RawMessage(`{"max":5}`))),
		entroq.PuttingDocInto("cfg", entroq.WithKeys("flags", ""), entroq.WithRawContent(json.RawMessage(`{"on":true}`))),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return &Gateway{client: client, config: &Config{}}
}

// TestReadTasks checks the read a worker most wants: look at the queue it is
// serving without claiming anything.
func TestReadTasks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := readFixture(ctx, t)

	reply, err := g.read(ctx, &Request{
		Type:       ReqTasks,
		TasksQuery: &wireTasksReq{&pb.TasksRequest{Queue: "inbox"}},
	})
	if err != nil {
		t.Fatalf("read tasks: %v", err)
	}
	if reply.Type != RespTasks {
		t.Errorf("reply type = %q, want %q", reply.Type, RespTasks)
	}
	if len(reply.Tasks) != 2 {
		t.Fatalf("got %d tasks, want the 2 in inbox", len(reply.Tasks))
	}
	// A read answers in protojson of api/entroq.proto, like everything else on
	// this wire, so a value comes back as JSON rather than base64.
	raw, err := json.Marshal(reply.Tasks)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := string(raw); !strings.Contains(got, `"queue":"inbox"`) {
		t.Errorf("encoded tasks do not look like protojson:\n%s", got)
	}

	t.Run("omit_values leaves the metadata", func(t *testing.T) {
		reply, err := g.read(ctx, &Request{
			Type:       ReqTasks,
			TasksQuery: &wireTasksReq{&pb.TasksRequest{Queue: "inbox", OmitValues: true}},
		})
		if err != nil {
			t.Fatalf("read tasks: %v", err)
		}
		if len(reply.Tasks) != 2 {
			t.Fatalf("got %d tasks, want 2", len(reply.Tasks))
		}
		for _, task := range reply.Tasks {
			if task.GetQueue() != "inbox" {
				t.Errorf("task queue = %q, want inbox", task.GetQueue())
			}
		}
	})

	t.Run("limit", func(t *testing.T) {
		reply, err := g.read(ctx, &Request{
			Type:       ReqTasks,
			TasksQuery: &wireTasksReq{&pb.TasksRequest{Queue: "inbox", Limit: 1}},
		})
		if err != nil {
			t.Fatalf("read tasks: %v", err)
		}
		if len(reply.Tasks) != 1 {
			t.Errorf("got %d tasks, want the 1 the limit allows", len(reply.Tasks))
		}
	})
}

// TestReadDocs covers the other read a worker reaches for: a config doc it
// needs but does not hold.
func TestReadDocs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := readFixture(ctx, t)

	reply, err := g.read(ctx, &Request{
		Type:      ReqDocs,
		DocsQuery: &wireDocsReq{&pb.DocsRequest{Query: &pb.DocQuery{Namespace: "cfg"}}},
	})
	if err != nil {
		t.Fatalf("read docs: %v", err)
	}
	if reply.Type != RespDocs {
		t.Errorf("reply type = %q, want %q", reply.Type, RespDocs)
	}
	if len(reply.Docs) != 2 {
		t.Fatalf("got %d docs, want the 2 in cfg", len(reply.Docs))
	}

	t.Run("by key", func(t *testing.T) {
		reply, err := g.read(ctx, &Request{
			Type: ReqDocs,
			DocsQuery: &wireDocsReq{&pb.DocsRequest{Query: &pb.DocQuery{
				Namespace: "cfg",
				KeyExact:  "limits",
			}}},
		})
		if err != nil {
			t.Fatalf("read docs: %v", err)
		}
		if len(reply.Docs) != 1 {
			t.Fatalf("got %d docs, want the 1 keyed limits", len(reply.Docs))
		}
		if got := reply.Docs[0].GetKey(); got != "limits" {
			t.Errorf("doc key = %q, want limits", got)
		}
	})
}

// TestReadQueuesAndNamespaces covers the two listing reads, which ask the same
// question of different things and so share one query message.
func TestReadQueuesAndNamespaces(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := readFixture(ctx, t)

	t.Run("queues", func(t *testing.T) {
		reply, err := g.read(ctx, &Request{Type: ReqQueues})
		if err != nil {
			t.Fatalf("read queues: %v", err)
		}
		if reply.Type != RespQueues {
			t.Errorf("reply type = %q, want %q", reply.Type, RespQueues)
		}
		counts := map[string]int32{}
		for _, q := range reply.Queues {
			counts[q.GetName()] = q.GetNumTasks()
		}
		if counts["inbox"] != 2 || counts["other"] != 1 {
			t.Errorf("queue counts = %v, want inbox=2 other=1", counts)
		}
	})

	t.Run("queues filtered by exact name", func(t *testing.T) {
		reply, err := g.read(ctx, &Request{
			Type:       ReqQueues,
			MatchQuery: &wireMatchReq{&pb.QueuesRequest{MatchExact: []string{"inbox"}}},
		})
		if err != nil {
			t.Fatalf("read queues: %v", err)
		}
		if len(reply.Queues) != 1 || reply.Queues[0].GetName() != "inbox" {
			t.Errorf("got %d queues, want only inbox", len(reply.Queues))
		}
	})

	t.Run("namespaces", func(t *testing.T) {
		// NamespaceStats is on entroq.Reader for this: a gateway client holds
		// the whole read surface a worker may ask about.
		reply, err := g.read(ctx, &Request{Type: ReqNamespaces})
		if err != nil {
			t.Fatalf("read namespaces: %v", err)
		}
		if reply.Type != RespNamespaces {
			t.Errorf("reply type = %q, want %q", reply.Type, RespNamespaces)
		}
		found := false
		for _, ns := range reply.Namespaces {
			if ns.GetName() == "cfg" {
				found = true
				if ns.GetNumDocs() != 2 {
					t.Errorf("cfg has %d docs, want 2", ns.GetNumDocs())
				}
			}
		}
		if !found {
			t.Errorf("namespaces do not include cfg: %v", reply.Namespaces)
		}
	})
}

// TestReadTime answers from the server's clock, which is the only clock a
// client should compare an observed arrival against.
func TestReadTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := readFixture(ctx, t)

	before := time.Now().UnixMilli()
	reply, err := g.read(ctx, &Request{Type: ReqTime})
	if err != nil {
		t.Fatalf("read time: %v", err)
	}
	if reply.Type != RespTime {
		t.Errorf("reply type = %q, want %q", reply.Type, RespTime)
	}
	if reply.TimeMs < before {
		t.Errorf("time_ms = %d, want at least %d", reply.TimeMs, before)
	}
}

// TestReadRefusesWhatItCannotAnswer checks that a read the gateway cannot act
// on is named rather than guessed at. A tasks query with neither a queue nor
// task IDs is the one that matters: it would otherwise mean "every task
// everywhere", which is not what a client that forgot a field meant.
func TestReadRefusesWhatItCannotAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := readFixture(ctx, t)

	for name, msg := range map[string]*Request{
		"tasks with no query":       {Type: ReqTasks},
		"docs with no query":        {Type: ReqDocs},
		"tasks naming nothing":      {Type: ReqTasks, TasksQuery: &wireTasksReq{&pb.TasksRequest{}}},
		"docs naming no namespace":  {Type: ReqDocs, DocsQuery: &wireDocsReq{&pb.DocsRequest{Query: &pb.DocQuery{}}}},
		"a type that is not a read": {Type: ReqModify},
	} {
		t.Run(name, func(t *testing.T) {
			if reply, err := g.read(ctx, msg); err == nil {
				t.Errorf("read answered %s with %+v, want a refusal", name, reply)
			}
		})
	}
}

// TestReadFailureDoesNotEndTheSession checks that a failed read is reported as
// a read failing, not as the session failing: the client hears why, and the
// task it is holding is untouched.
func TestReadFailureDoesNotEndTheSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := readFixture(ctx, t)

	sent := &fakeConn{}
	g.conn = sent
	if err := g.handleReadAndRespond(ctx, &Request{Type: ReqTasks}); err != nil {
		t.Fatalf("handleReadAndRespond returned %v; a failed read must not fail the session", err)
	}
	if len(sent.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent.sent))
	}
	reply := sent.sent[0]
	if reply.Type != RespError {
		t.Errorf("reply type = %q, want %q", reply.Type, RespError)
	}
	if reply.Message == "" {
		t.Error("the refusal says nothing about why")
	}
	if reply.Class != ExitCaller.String() && reply.Class != ExitGateway.String() {
		t.Errorf("reply class = %q, want a class a client can branch on", reply.Class)
	}
}

// fakeConn records what the gateway sends and answers nothing, which is all a
// read needs: a read is not a turn, so nothing comes back.
type fakeConn struct {
	sent []*Response
}

func (c *fakeConn) Send(_ context.Context, msg *Response) error {
	c.sent = append(c.sent, msg)
	return nil
}

func (c *fakeConn) Recv(ctx context.Context) (*Request, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (c *fakeConn) Close() error { return nil }
