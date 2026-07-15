package workgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/pbconv"
	"github.com/shiblon/entroq/pkg/worker"
	"google.golang.org/protobuf/types/known/structpb"
)

// These tests drive the gateway Bridge against a real in-memory EntroQ while the
// test itself plays the worker on the other end of an in-memory pipe. Because
// the test is in package workgateway, it builds and reads the wire types
// directly, so the assertions double as documentation of the exact protocol.
// Every domain object on the wire is the protojson of an api/entroq.proto
// message (a pb.Task, pb.Doc, pb.ModifyRequest, pb.ModifyDep), exactly what a
// foreign worker generates from the proto; the test constructs those pb messages
// the same way a foreign worker would. Registration is out-of-band (a Config
// passed to Run), so the tests never send a wire register message.

// codec is the worker side of the wire in a test: send a message, receive the
// next one.
type codec struct {
	t   *testing.T
	enc *json.Encoder
	dec *json.Decoder
}

func (c *codec) send(v any) {
	c.t.Helper()
	if err := c.enc.Encode(v); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

func (c *codec) recv(v any) {
	c.t.Helper()
	if err := c.dec.Decode(v); err != nil {
		c.t.Fatalf("recv: %v", err)
	}
}

// session runs a Bridge over pipes against eq with the given registration, and
// the test plays the worker.
type session struct {
	t       *testing.T
	c       *codec
	cancel  context.CancelFunc
	errc    chan error
	clientR *io.PipeReader // client's read end; closing it breaks the bridge's Send
	clientW *io.PipeWriter // client's write end; closing it gives the bridge's Recv EOF
}

func newSession(t *testing.T, ctx context.Context, eq *entroq.EntroQ, cfg Config, lease time.Duration) *session {
	t.Helper()
	clientR, gatewayW := io.Pipe()
	gatewayR, clientW := io.Pipe()
	bridge := NewBridge(NewPipeConn(gatewayR, gatewayW), WithConfig(cfg), WithLease(lease))

	rctx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- bridge.Run(rctx, eq) }()

	return &session{
		t:       t,
		c:       &codec{t: t, enc: json.NewEncoder(clientW), dec: json.NewDecoder(clientR)},
		cancel:  cancel,
		errc:    errc,
		clientR: clientR,
		clientW: clientW,
	}
}

// closeClient simulates the worker vanishing: it closes both client pipe ends,
// so whichever operation the bridge is blocked on next fails as it would on a
// dropped connection (Recv sees EOF, Send sees a closed pipe).
func (s *session) closeClient() {
	s.t.Helper()
	s.clientW.Close()
	s.clientR.Close()
}

// stop cancels the bridge and asserts it exited cleanly (nil or a cancellation).
// Use it for tests where the loop keeps running until the test is done with it.
func (s *session) stop() {
	s.t.Helper()
	s.cancel()
	if err := <-s.errc; err != nil && !errors.Is(err, context.Canceled) {
		s.t.Errorf("bridge run: %v", err)
	}
}

// wait returns the bridge's Run error, for tests where the worker is expected to
// stop on its own (a fatal or a bad registration).
func (s *session) wait() error {
	s.t.Helper()
	select {
	case err := <-s.errc:
		return err
	case <-time.After(5 * time.Second):
		s.t.Fatal("bridge did not exit on its own")
		return nil
	}
}

func newEQ(t *testing.T, ctx context.Context) *entroq.EntroQ {
	t.Helper()
	eq, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	t.Cleanup(func() { eq.Close() })
	return eq
}

func insertTask(t *testing.T, ctx context.Context, eq *entroq.EntroQ, q, value string) {
	t.Helper()
	if _, err := eq.Modify(ctx, entroq.InsertingInto(q, entroq.WithValue(value))); err != nil {
		t.Fatalf("insert into %q: %v", q, err)
	}
}

// workCfg is the common registration: serve "in" with a work handler.
func workCfg(queues ...string) Config {
	if len(queues) == 0 {
		queues = []string{"in"}
	}
	return Config{Queues: queues, Work: true}
}

// okResult builds an "ok" result committing mr (which may be nil for an empty
// commit). It is the common reply a worker sends after doWork.
func okResult(mr *pb.ModifyRequest) result {
	r := result{Type: msgResult, disposition: disposition{Outcome: outcomeOK}}
	if mr != nil {
		r.Modification = &wireModReq{mr}
	}
	return r
}

// deleteTask builds a ModifyRequest that deletes the given task, the usual "I
// consumed this" commit. A wire worker names the task by the id/version it saw
// in doWork; the gateway fixes the version up to the stable claimed value.
func deleteTask(t *pb.Task) *pb.ModifyRequest {
	return &pb.ModifyRequest{Deletes: []*pb.TaskID{{Id: t.Id, Version: t.Version, Queue: t.Queue}}}
}

// echoData builds the TaskData for a change that echoes the received task: it
// carries the task's full current state, so the worker edits the fields it wants
// on the returned copy and the rest is preserved. This mirrors how a foreign
// worker restates a task it received in doWork.
func echoData(t *pb.Task) *pb.TaskData {
	return &pb.TaskData{
		Queue:   t.Queue,
		AtMs:    t.AtMs,
		Value:   t.Value,
		Attempt: t.Attempt,
		Err:     t.Err,
	}
}

// TestBridge_OKDeletes is the happy path: receive the task, reply ok with a
// delete of the claimed task, and confirm the queue drains.
func TestBridge_OKDeletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	if dw.Type != msgDoWork {
		t.Fatalf("got %q, want %q", dw.Type, msgDoWork)
	}
	if got := dw.Task.Value.GetStringValue(); got != "hello" {
		t.Errorf("task value = %q, want %q", got, "hello")
	}
	s.c.send(okResult(deleteTask(dw.Task.Task)))

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("wait queue empty: %v", err)
	}
	s.stop()
}

// TestBridge_OKEmptyDoesNotDelete locks in the core faithfulness rule: "ok" means
// only "no error". With an absent modification the claimed task is committed
// unchanged and therefore stays in its queue (leased until expiry), exactly as a
// Go DoModify that returns no mods.
func TestBridge_OKEmptyDoesNotDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(okResult(nil)) // no modification

	// Give the empty commit time to happen, then confirm the task is still there.
	time.Sleep(200 * time.Millisecond)
	tasks, err := eq.Tasks(ctx, "in")
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("queue has %d tasks, want 1: an empty ok must not delete the task", len(tasks))
	}
	s.stop()
}

// TestBridge_Insert covers producing work: the result inserts a new task into
// another queue and deletes the input in one atomic modification.
func TestBridge_Insert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	mr := deleteTask(dw.Task.Task)
	mr.Inserts = []*pb.TaskData{{Queue: "out", Value: structpb.NewStringValue("produced")}}
	s.c.send(okResult(mr))

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("wait input drained: %v", err)
	}
	out, err := eq.Tasks(ctx, "out")
	if err != nil {
		t.Fatalf("tasks out: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("out has %d tasks, want 1", len(out))
	}
	if got := string(out[0].Value); got != `"produced"` {
		t.Errorf("produced value = %s, want %q", got, `"produced"`)
	}
	s.stop()
}

// TestBridge_ChangeMovePreservesID moves the claimed task to another queue via a
// change. The ID stays the same (only the version bumps), and the value is
// preserved because the change echoes the received task's full state.
func TestBridge_ChangeMovePreservesID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	// Echo the task, override only the destination queue: a move sets NewData's
	// queue different from OldId's (the source).
	nd := echoData(dw.Task.Task)
	nd.Queue = "moved"
	mr := &pb.ModifyRequest{Changes: []*pb.TaskChange{{
		OldId:   &pb.TaskID{Id: dw.Task.Id, Version: dw.Task.Version, Queue: dw.Task.Queue},
		NewData: nd,
	}}}
	s.c.send(okResult(mr))

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("input not drained by move: %v", err)
	}
	moved, err := eq.Tasks(ctx, "moved")
	if err != nil {
		t.Fatalf("tasks moved: %v", err)
	}
	if len(moved) != 1 {
		t.Fatalf("moved has %d tasks, want 1", len(moved))
	}
	if moved[0].ID != dw.Task.Id {
		t.Errorf("moved task ID = %q, want %q: a move must preserve the ID", moved[0].ID, dw.Task.Id)
	}
	if got := string(moved[0].Value); got != `"hello"` {
		t.Errorf("moved value = %s, want %q: an echoed field must be preserved", got, `"hello"`)
	}
	if moved[0].Version <= dw.Task.Version {
		t.Errorf("moved version = %d, want > %d: a move must bump the version", moved[0].Version, dw.Task.Version)
	}
	s.stop()
}

// TestBridge_ChangeDefers changes the claimed task's arrival time (leaving it in
// place), the fundamental "run this later" operation. The task stays in its
// queue, keeps its ID and value, and is no longer immediately due.
func TestBridge_ChangeDefers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	future := time.Now().Add(time.Hour)
	nd := echoData(dw.Task.Task)
	nd.AtMs = pbconv.ToMS(future) // defer: push the arrival time out, no move
	mr := &pb.ModifyRequest{Changes: []*pb.TaskChange{{
		OldId:   &pb.TaskID{Id: dw.Task.Id, Version: dw.Task.Version, Queue: dw.Task.Queue},
		NewData: nd,
	}}}
	s.c.send(okResult(mr))

	// The task stays in "in" but deferred, so it is not immediately reclaimed.
	time.Sleep(200 * time.Millisecond)
	tasks, err := eq.Tasks(ctx, "in")
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("in has %d tasks, want 1 (deferred, still present)", len(tasks))
	}
	if tasks[0].ID != dw.Task.Id {
		t.Errorf("deferred task ID = %q, want %q", tasks[0].ID, dw.Task.Id)
	}
	if got := string(tasks[0].Value); got != `"hello"` {
		t.Errorf("deferred value = %s, want %q", got, `"hello"`)
	}
	if !tasks[0].At.After(time.Now().Add(30 * time.Minute)) {
		t.Errorf("deferred At = %v, want well into the future", tasks[0].At)
	}
	s.stop()
}

// TestBridge_ChangeSetsValue shows a change carries the full desired state: the
// committed value is the one on NewData, not the original, so the gateway does
// not force-preserve. Clearing a field works the same way, by sending it empty.
func TestBridge_ChangeSetsValue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	nd := echoData(dw.Task.Task)
	nd.Value = structpb.NewStringValue("changed") // set a new value
	nd.Queue = "moved"                            // move too, so it lands where this worker won't re-claim
	mr := &pb.ModifyRequest{Changes: []*pb.TaskChange{{
		OldId:   &pb.TaskID{Id: dw.Task.Id, Version: dw.Task.Version, Queue: dw.Task.Queue},
		NewData: nd,
	}}}
	s.c.send(okResult(mr))

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("input not drained: %v", err)
	}
	moved, err := eq.Tasks(ctx, "moved")
	if err != nil {
		t.Fatalf("tasks moved: %v", err)
	}
	if len(moved) != 1 {
		t.Fatalf("moved has %d tasks, want 1", len(moved))
	}
	if got := string(moved[0].Value); got != `"changed"` {
		t.Errorf("moved value = %s, want %q: a change must commit the new value", got, `"changed"`)
	}
	s.stop()
}

// TestBridge_TakeDocs exercises the optional doc-claim phase: the worker asks for
// a doc, the gateway claims it and passes it into doWork.
func TestBridge_TakeDocs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	if _, err := eq.Modify(ctx,
		entroq.InsertingInto("in", entroq.WithValue("hello")),
		entroq.PuttingDoc(&entroq.DocData{Namespace: "ns", Key: "k", Content: json.RawMessage(`"docval"`)}),
	); err != nil {
		t.Fatalf("insert task+doc: %v", err)
	}

	cfg := workCfg()
	cfg.TakeDocs = true
	s := newSession(t, ctx, eq, cfg, time.Second)

	var td takeDocsMsg
	s.c.recv(&td)
	if td.Type != msgTakeDocs {
		t.Fatalf("got %q, want %q", td.Type, msgTakeDocs)
	}
	s.c.send(docsMsg{Type: msgDocs, Claims: []docClaim{{Namespace: "ns", Key: "k"}}})

	var dw doWorkMsg
	s.c.recv(&dw)
	if len(dw.Docs) != 1 {
		t.Fatalf("doWork carried %d docs, want 1", len(dw.Docs))
	}
	if dw.Docs[0].Key != "k" {
		t.Errorf("doc key = %q, want %q", dw.Docs[0].Key, "k")
	}
	s.c.send(okResult(deleteTask(dw.Task.Task)))

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("wait queue empty: %v", err)
	}
	s.stop()
}

// TestBridge_DocInsert covers writing a doc back: the result inserts a new doc
// and deletes the input task in one atomic modification.
func TestBridge_DocInsert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	mr := deleteTask(dw.Task.Task)
	mr.DocInserts = []*pb.DocData{{Namespace: "ns", Key: "k", Content: structpb.NewStringValue("docval")}}
	s.c.send(okResult(mr))

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("wait input drained: %v", err)
	}
	docs, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: "ns"})
	if err != nil {
		t.Fatalf("docs: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("ns has %d docs, want 1", len(docs))
	}
	if docs[0].Key != "k" {
		t.Errorf("doc key = %q, want %q", docs[0].Key, "k")
	}
	if got := string(docs[0].Content); got != `"docval"` {
		t.Errorf("doc content = %s, want %q", got, `"docval"`)
	}
	s.stop()
}

// TestBridge_DocChange claims a doc via takeDocs, then updates its content with a
// doc change that echoes the received doc's identity and keys, the doc analog of
// a task change.
func TestBridge_DocChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	if _, err := eq.Modify(ctx,
		entroq.InsertingInto("in", entroq.WithValue("hello")),
		entroq.PuttingDoc(&entroq.DocData{Namespace: "ns", Key: "k", Content: json.RawMessage(`"old"`)}),
	); err != nil {
		t.Fatalf("insert task+doc: %v", err)
	}

	cfg := workCfg()
	cfg.TakeDocs = true
	s := newSession(t, ctx, eq, cfg, time.Second)

	var td takeDocsMsg
	s.c.recv(&td)
	s.c.send(docsMsg{Type: msgDocs, Claims: []docClaim{{Namespace: "ns", Key: "k"}}})

	var dw doWorkMsg
	s.c.recv(&dw)
	if len(dw.Docs) != 1 {
		t.Fatalf("doWork carried %d docs, want 1", len(dw.Docs))
	}
	doc := dw.Docs[0]
	mr := deleteTask(dw.Task.Task)
	mr.DocChanges = []*pb.DocChange{{
		OldId: &pb.DocID{Namespace: doc.Namespace, Id: doc.Id, Version: doc.Version},
		NewData: &pb.DocData{
			Namespace:    doc.Namespace,
			Key:          doc.Key, // keys are immutable; echo them
			SecondaryKey: doc.SecondaryKey,
			Content:      structpb.NewStringValue("new"), // the edit
		},
	}}
	s.c.send(okResult(mr))

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("wait input drained: %v", err)
	}
	docs, err := eq.Docs(ctx, &entroq.DocQuery{Namespace: "ns"})
	if err != nil {
		t.Fatalf("docs: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("ns has %d docs, want 1", len(docs))
	}
	if got := string(docs[0].Content); got != `"new"` {
		t.Errorf("doc content = %s, want %q (change must update content)", got, `"new"`)
	}
	s.stop()
}

// TestBridge_DependencyMoves exercises the dependency phase: the commit depends
// on a task that does not exist, so it fails a dependency check. The gateway
// reports the failed dependencies and the worker, seeing its own task was not
// implicated, moves it to an error queue. The move lands because the claimed task
// is still validly held.
func TestBridge_DependencyMoves(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	cfg := workCfg()
	cfg.Dependency = true // opt in to the dependency phase
	s := newSession(t, ctx, eq, cfg, time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	// Delete our task, but also depend on a task that does not exist: the whole
	// modification fails atomically on the missing dependency, and our task is
	// not implicated.
	const missingID = "00000000-0000-0000-0000-000000000000"
	mr := deleteTask(dw.Task.Task)
	mr.Depends = []*pb.TaskID{{Id: missingID, Version: 0, Queue: "in"}}
	s.c.send(okResult(mr))

	// The commit fails, so the gateway reports the failed dependencies.
	var dep dependencyMsg
	s.c.recv(&dep)
	if dep.Type != msgDependency {
		t.Fatalf("got %q, want %q", dep.Type, msgDependency)
	}
	// A DETAIL entry carries the message; a DEPEND entry names the missing task.
	var sawDetail, sawMissing bool
	for _, d := range dep.Deps {
		switch d.Type {
		case pb.ActionType_DETAIL:
			sawDetail = true
		case pb.ActionType_DEPEND:
			if d.Id.GetId() == missingID {
				sawMissing = true
			}
		}
	}
	if !sawDetail {
		t.Errorf("dependency report carried no DETAIL entry: %+v", dep.Deps)
	}
	if !sawMissing {
		t.Errorf("dependency report did not name the missing depend %q: %+v", missingID, dep.Deps)
	}

	// Our task was not implicated, so a move disposition lands.
	s.c.send(done{Type: msgDone, disposition: disposition{Outcome: outcomeMove, To: "dead", Message: "dependency vanished"}})

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("input not drained by the dependency move: %v", err)
	}
	dead, err := eq.Tasks(ctx, "dead")
	if err != nil {
		t.Fatalf("tasks dead: %v", err)
	}
	if len(dead) != 1 {
		t.Fatalf("dead has %d tasks, want 1: the dependency move must land", len(dead))
	}
	s.stop()
}

// TestBridge_Ack exercises the delete shorthand: with ack set and no explicit
// disposition of the input task, the gateway deletes the claimed task, so the
// queue drains without the worker echoing the task id/version into a delete.
func TestBridge_Ack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	// Ack alone, no modification at all: the gateway synthesizes the delete.
	s.c.send(result{Type: msgResult, disposition: disposition{Outcome: outcomeOK}, Ack: true})

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("ack did not delete the claimed task: %v", err)
	}
	s.stop()
}

// TestBridge_AckComposesWithInsert shows ack riding along with other work: the
// worker inserts a new task and acks the input in one atomic commit, without
// building the input's delete by hand.
func TestBridge_AckComposesWithInsert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	mr := &pb.ModifyRequest{Inserts: []*pb.TaskData{{Queue: "out", Value: structpb.NewStringValue("produced")}}}
	r := okResult(mr)
	r.Ack = true
	s.c.send(r)

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("ack did not drain the input alongside the insert: %v", err)
	}
	out, err := eq.Tasks(ctx, "out")
	if err != nil {
		t.Fatalf("tasks out: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("out has %d tasks, want 1", len(out))
	}
	s.stop()
}

// TestBridge_AckModifyWins proves the "modify wins" rule: when ack is set but the
// modification already disposes of the claimed task (here a move), the explicit
// op stands and the ack-delete is suppressed, so the task is moved, not deleted.
func TestBridge_AckModifyWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	nd := echoData(dw.Task.Task)
	nd.Queue = "moved"
	mr := &pb.ModifyRequest{Changes: []*pb.TaskChange{{
		OldId:   &pb.TaskID{Id: dw.Task.Id, Version: dw.Task.Version, Queue: dw.Task.Queue},
		NewData: nd,
	}}}
	r := okResult(mr)
	r.Ack = true // ignored: the change already disposes of the input
	s.c.send(r)

	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("input not drained by the move: %v", err)
	}
	moved, err := eq.Tasks(ctx, "moved")
	if err != nil {
		t.Fatalf("tasks moved: %v", err)
	}
	if len(moved) != 1 {
		t.Fatalf("moved has %d tasks, want 1: modify must win over ack (the task is moved, not deleted)", len(moved))
	}
	if got := string(moved[0].Value); got != `"hello"` {
		t.Errorf("moved value = %s, want %q", got, `"hello"`)
	}
	s.stop()
}

// TestBridge_Success exercises the optional success phase: after the commit, the
// gateway sends success and the worker acknowledges with done.
func TestBridge_Success(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	cfg := workCfg()
	cfg.Success = true
	s := newSession(t, ctx, eq, cfg, time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(okResult(deleteTask(dw.Task.Task)))

	var su successMsg
	s.c.recv(&su)
	if su.Type != msgSuccess {
		t.Fatalf("got %q, want %q", su.Type, msgSuccess)
	}
	// The success phase runs after the commit, so the task is already gone.
	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("wait queue empty: %v", err)
	}
	s.c.send(done{Type: msgDone})
	s.stop()
}

// TestBridge_SuccessFatal proves a fatal reply to the success phase stops the
// worker, and only after the task has already committed.
func TestBridge_SuccessFatal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	cfg := workCfg()
	cfg.Success = true
	s := newSession(t, ctx, eq, cfg, time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(okResult(deleteTask(dw.Task.Task)))
	var su successMsg
	s.c.recv(&su)
	s.c.send(done{Type: msgDone, disposition: disposition{Outcome: outcomeFatal, Message: "success step blew up"}})

	// A worker-requested fatal is a caller fault, reported before the stop.
	var em errorMsg
	s.c.recv(&em)
	if em.Class != ExitCaller.String() {
		t.Errorf("error class = %q, want %q", em.Class, ExitCaller.String())
	}
	ee, ok := AsExit(s.wait())
	if !ok || ee.Class != ExitCaller {
		t.Fatalf("expected a caller-fault exit, got %+v (ok=%v)", ee, ok)
	}
	// The task committed before the fatal, so the queue is empty.
	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("task should have committed before the fatal: %v", err)
	}
}

// TestBridge_RetryMoves checks the retry outcome with quarantine: with
// maxAttempts=1 an orMove retry lands the task in the error queue on first
// failure.
func TestBridge_RetryMoves(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	cfg := workCfg()
	cfg.MaxAttempts = 1
	s := newSession(t, ctx, eq, cfg, time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(result{Type: msgResult, disposition: disposition{Outcome: outcomeRetry, Message: "please retry", OrMove: "dead"}})

	deadline := time.After(5 * time.Second)
	for {
		tasks, err := eq.Tasks(ctx, "dead")
		if err != nil {
			t.Fatalf("tasks: %v", err)
		}
		if len(tasks) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("task never quarantined to the error queue")
		case <-time.After(20 * time.Millisecond):
		}
	}
	s.stop()
}

// TestBridge_Fatal checks that a fatal outcome from doWork stops the worker.
func TestBridge_Fatal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(result{Type: msgResult, disposition: disposition{Outcome: outcomeFatal, Message: "boom"}})

	// A worker-requested fatal is a caller fault, reported before the stop.
	var em errorMsg
	s.c.recv(&em)
	if em.Class != ExitCaller.String() {
		t.Errorf("error class = %q, want %q", em.Class, ExitCaller.String())
	}
	ee, ok := AsExit(s.wait())
	if !ok || ee.Class != ExitCaller {
		t.Fatalf("expected a caller-fault exit, got %+v (ok=%v)", ee, ok)
	}
}

// TestBridge_RequiresQueue rejects a registration with no queues.
func TestBridge_RequiresQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)

	s := newSession(t, ctx, eq, Config{Work: true}, time.Second) // no queues
	if err := s.wait(); err == nil {
		t.Fatal("expected an error for a registration with no queues")
	}
}

// TestBridge_RequiresWorkHandler rejects a registration with no work handler:
// the gateway would have nothing to do.
func TestBridge_RequiresWorkHandler(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)

	s := newSession(t, ctx, eq, Config{Queues: []string{"in"}}, time.Second) // Work:false
	if err := s.wait(); err == nil {
		t.Fatal("expected an error for a registration with no work handler")
	}
}

// The tests below exercise transport failure: a worker that drops its connection
// mid-task. Because the gateway owns the claim under a lease rather than a
// commit, a drop leaves the task reclaimable after the lease -- the at-least-once
// guarantee that lets a foreign worker crash without losing work. A dropped
// connection is a *clean* stop (Run returns nil): the client owns its own
// lifecycle, so there is nothing for the gateway to alert on. A decodable message
// of the wrong type or shape, by contrast, is a caller fault the gateway reports
// over the error channel before it stops.

// TestBridge_ClientDropReclaims is the core resilience property: a worker that
// receives a task then vanishes without replying leaves the bridge to stop
// cleanly, the (uncommitted) task still in its queue, and the task reclaimable
// once the lease lapses.
func TestBridge_ClientDropReclaims(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	lease := 200 * time.Millisecond
	s := newSession(t, ctx, eq, workCfg(), lease)

	var dw doWorkMsg
	s.c.recv(&dw) // the task is now claimed by the bridge
	s.closeClient()

	// A dropped client is a clean stop: the client owns its own lifecycle.
	if err := s.wait(); err != nil {
		t.Fatalf("a client drop should be a clean stop, got: %v", err)
	}
	// The task was never committed, so it is still present, just leased.
	tasks, err := eq.Tasks(ctx, "in")
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("task lost after a drop: got %d tasks, want 1", len(tasks))
	}
	// Once the lease lapses it must be claimable again (at-least-once).
	deadline := time.After(5 * time.Second)
	for {
		claimed, err := eq.TryClaim(ctx, entroq.From("in"), entroq.ClaimFor(time.Second))
		if err != nil {
			t.Fatalf("try claim: %v", err)
		}
		if claimed != nil {
			if got := string(claimed.Value); got != `"hello"` {
				t.Errorf("reclaimed value = %s, want %q", got, `"hello"`)
			}
			return // reclaimed: at-least-once holds
		}
		select {
		case <-deadline:
			t.Fatal("task was never reclaimable after the lease expired")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestBridge_ClientDropDuringTakeDocs covers a drop at the takeDocs phase: the
// bridge stops cleanly rather than hanging, before any work happens.
func TestBridge_ClientDropDuringTakeDocs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	cfg := workCfg()
	cfg.TakeDocs = true
	s := newSession(t, ctx, eq, cfg, time.Second)

	var td takeDocsMsg
	s.c.recv(&td) // the bridge is now waiting for the docs reply
	s.closeClient()

	if err := s.wait(); err != nil {
		t.Fatalf("a client drop during takeDocs should be a clean stop, got: %v", err)
	}
}

// TestBridge_SuccessDropStopsWorker proves a client dropping during the
// best-effort success phase stops the worker promptly -- rather than looping back
// to claim a task the dead connection cannot deliver, which would needlessly
// starve that task for a lease period -- and that the already-committed task
// stays committed (the exactly-once boundary holds). The drop is a clean stop.
func TestBridge_SuccessDropStopsWorker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	cfg := workCfg()
	cfg.Success = true
	s := newSession(t, ctx, eq, cfg, time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(okResult(deleteTask(dw.Task.Task))) // commit: delete the task

	var su successMsg
	s.c.recv(&su)   // the commit has happened; the bridge now awaits the done reply
	s.closeClient() // drop before acknowledging the success phase

	// The dead connection stops the worker cleanly (no wasteful reclaim loop).
	if err := s.wait(); err != nil {
		t.Fatalf("a success-phase drop should be a clean stop, got: %v", err)
	}
	// ...and the commit is durable regardless.
	if err := eq.WaitQueuesEmpty(ctx, entroq.MatchExact("in")); err != nil {
		t.Fatalf("commit must survive a success-phase drop: %v", err)
	}
}

// TestBridge_WrongMessageType is a caller fault: a decodable reply of an
// unexpected type is a client bug, so the gateway reports it over the error
// channel (class "caller") and stops with a caller-fault exit.
func TestBridge_WrongMessageType(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	s.c.send(map[string]string{"type": "bogus"}) // decodable, but not a result

	// The gateway reports the fault over the error channel before stopping. The
	// client must read it (the pipe is synchronous), which also lets us assert it.
	var em errorMsg
	s.c.recv(&em)
	if em.Type != msgError || em.Class != ExitCaller.String() {
		t.Fatalf("got %+v, want an error message of class %q", em, ExitCaller.String())
	}
	ee, ok := AsExit(s.wait())
	if !ok || ee.Class != ExitCaller {
		t.Fatalf("expected a caller-fault ExitError, got: %+v (ok=%v)", ee, ok)
	}
}

// TestBridge_MalformedInput treats undecodable input as a lost connection: the
// gateway cannot parse the stream, so it stops cleanly rather than hanging or
// panicking. (A decodable-but-wrong message is a caller fault; see
// TestBridge_WrongMessageType.)
func TestBridge_MalformedInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	eq := newEQ(t, ctx)
	insertTask(t, ctx, eq, "in", "hello")

	s := newSession(t, ctx, eq, workCfg(), time.Second)

	var dw doWorkMsg
	s.c.recv(&dw)
	if _, err := s.clientW.Write([]byte("this is not json\n")); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	if err := s.wait(); err != nil {
		t.Fatalf("malformed input should be a clean stop, got: %v", err)
	}
}

// TestClassify checks the exit classification in isolation, over the errors a
// worker run can surface.
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bridge *Bridge
		err    error
		want   ExitClass
	}{
		{"nil is clean", &Bridge{}, nil, ExitOK},
		{"canceled is clean", &Bridge{}, context.Canceled, ExitOK},
		{"lost connection is clean", &Bridge{connLost: true}, errors.New("anything"), ExitOK},
		{"unavailable is transient", &Bridge{}, entroq.Unavailablef("backend down"), ExitTransient},
		{"fatal is caller fault", &Bridge{}, worker.FatalErrorf("bad message"), ExitCaller},
		{"unknown is gateway fault", &Bridge{}, errors.New("surprise"), ExitGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.bridge.classify(tc.err); got != tc.want {
				t.Errorf("classify = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestExitClassCodes locks the class -> exit code and wire token mapping.
func TestExitClassCodes(t *testing.T) {
	for _, tc := range []struct {
		class ExitClass
		code  int
		token string
	}{
		{ExitOK, 0, "ok"},
		{ExitTransient, 75, "transient"},
		{ExitCaller, 78, "caller"},
		{ExitGateway, 70, "gateway"},
	} {
		if got := tc.class.ExitCode(); got != tc.code {
			t.Errorf("%v.ExitCode() = %d, want %d", tc.class, got, tc.code)
		}
		if got := tc.class.String(); got != tc.token {
			t.Errorf("%v.String() = %q, want %q", tc.class, got, tc.token)
		}
	}
}
