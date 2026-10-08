package workgateway

import (
	"context"
	"fmt"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/pbconv"
)

// Reads are what a worker does when the task it was handed is not the whole
// story: a config doc to look up, a queue to size, a sibling task to check.
//
// They are INTERLEAVED WITHIN A TURN rather than being turns of their own (see
// recv): a client asks as many questions as it likes while it decides, and the
// turn ends only when the answer the phase is waiting for arrives. So a read
// costs the client a round trip and costs the protocol nothing -- no state, no
// ordering rule, nothing for either side to track.
//
// Every read is the same request and response message the gRPC service uses.
// A client that generated its types once has them for both, and this package
// does not invent a query language.
//
// A read answers as the SESSION'S claimant, which is the same identity the
// worker holds its tasks as. There is no separate authorization here: a client
// that can commit a modification can already see what it would be committing
// against, so a gateway is only ever as trusted as the client it hosts.
func (g *Gateway) handleReadAndRespond(ctx context.Context, msg *RecvMessage) error {
	reply, err := g.read(ctx, msg)
	if err != nil {
		// A read that fails is the read failing, not the session: the client
		// hears why and decides, and the task in hand is untouched.
		reply = g.newSend(SendError, "")
		reply.Class = g.classify(err).String()
		reply.Message = err.Error()
	}
	if err := g.conn.Send(ctx, reply); err != nil {
		return fmt.Errorf("send %s: %w", reply.Type, err)
	}
	return nil
}

// read answers one read request. The reply carries no Expect, because a read
// is not a turn: whatever the phase was waiting for before the read is still
// what it is waiting for after it.
func (g *Gateway) read(ctx context.Context, msg *RecvMessage) (*SendMessage, error) {
	switch msg.Type {
	case RecvTime:
		t, err := g.client.Time(ctx)
		if err != nil {
			return nil, fmt.Errorf("read time: %w", err)
		}
		reply := g.newSend(SendTime, "")
		reply.TimeMs = t.UnixMilli()
		return reply, nil

	case RecvTasks:
		return g.readTasks(ctx, msg.TasksQuery)

	case RecvDocs:
		return g.readDocs(ctx, msg.DocsQuery)

	case RecvQueues:
		return g.readQueues(ctx, msg.MatchQuery)

	case RecvNamespaces:
		return g.readNamespaces(ctx, msg.MatchQuery)
	}
	return nil, fmt.Errorf("not a read: %q", msg.Type)
}

func (g *Gateway) readTasks(ctx context.Context, q *wireTasksReq) (*SendMessage, error) {
	if q == nil || q.TasksRequest == nil {
		return nil, fmt.Errorf("a %q read carries no tasks_query", RecvTasks)
	}
	req := q.TasksRequest
	// Validated the way the service validates it, so the same query is well
	// formed or refused wherever it is asked.
	if err := (&entroq.TasksQuery{Queue: req.GetQueue(), IDs: req.GetTaskId()}).Validate(); err != nil {
		return nil, fmt.Errorf("tasks query: %w", err)
	}
	// ClaimedBy and WithTaskID filter only when they are given something, so
	// an empty query is every task in the queue.
	opts := []entroq.TasksOpt{
		entroq.ClaimedBy(req.GetClaimantId()),
		entroq.WithTaskID(req.GetTaskId()...),
		entroq.LimitTasks(int(req.GetLimit())),
	}
	if req.GetOmitValues() {
		opts = append(opts, entroq.OmitValues())
	}
	tasks, err := g.client.Tasks(ctx, req.GetQueue(), opts...)
	if err != nil {
		return nil, fmt.Errorf("read tasks: %w", err)
	}
	reply := g.newSend(SendTasks, "")
	for _, task := range tasks {
		pt, err := pbconv.TaskToProto(task)
		if err != nil {
			return nil, fmt.Errorf("read tasks, task %s: %w", task.ID, err)
		}
		reply.Tasks = append(reply.Tasks, wireTask{pt})
	}
	return reply, nil
}

func (g *Gateway) readDocs(ctx context.Context, q *wireDocsReq) (*SendMessage, error) {
	if q == nil || q.DocsRequest == nil {
		return nil, fmt.Errorf("a %q read carries no docs_query", RecvDocs)
	}
	dq := q.GetQuery()
	query := &entroq.DocQuery{
		Namespace:  dq.GetNamespace(),
		IDs:        dq.GetIds(),
		KeyExact:   dq.GetKeyExact(),
		KeyStart:   dq.GetKeyStart(),
		KeyEnd:     dq.GetKeyEnd(),
		Limit:      int(dq.GetLimit()),
		OmitValues: dq.GetOmitValues(),
	}
	if err := query.Validate(); err != nil {
		return nil, fmt.Errorf("docs query: %w", err)
	}
	docs, err := g.client.Docs(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read docs: %w", err)
	}
	reply := g.newSend(SendDocs, "")
	for _, doc := range docs {
		pd, err := pbconv.DocToProto(doc)
		if err != nil {
			return nil, fmt.Errorf("read docs, doc %s/%s: %w", doc.Namespace, doc.ID, err)
		}
		reply.Docs = append(reply.Docs, wireDoc{pd})
	}
	return reply, nil
}

func (g *Gateway) readQueues(ctx context.Context, q *wireMatchReq) (*SendMessage, error) {
	counts, err := g.client.Queues(ctx, matchOpts(q)...)
	if err != nil {
		return nil, fmt.Errorf("read queues: %w", err)
	}
	reply := g.newSend(SendQueues, "")
	for name, count := range counts {
		reply.Queues = append(reply.Queues, wireQueueStats{&pb.QueueStats{
			Name:     name,
			NumTasks: int32(count),
		}})
	}
	return reply, nil
}

func (g *Gateway) readNamespaces(ctx context.Context, q *wireMatchReq) (*SendMessage, error) {
	stats, err := g.client.NamespaceStats(ctx, matchOpts(q)...)
	if err != nil {
		return nil, fmt.Errorf("read namespaces: %w", err)
	}
	reply := g.newSend(SendNamespaces, "")
	for _, stat := range stats {
		reply.Namespaces = append(reply.Namespaces, wireNamespaceStat{&pb.NamespaceStat{
			Name:       stat.Name,
			NumDocs:    int32(stat.Size),
			NumClaimed: int32(stat.Claimed),
		}})
	}
	return reply, nil
}

// matchOpts is the query queues and namespaces share: they ask the same
// question of different things. A missing query is no filter at all, which
// lists everything up to the backend's own limit.
func matchOpts(q *wireMatchReq) []entroq.QueuesOpt {
	if q == nil || q.QueuesRequest == nil {
		return nil
	}
	return []entroq.QueuesOpt{
		entroq.MatchPrefix(q.GetMatchPrefix()...),
		entroq.MatchExact(q.GetMatchExact()...),
		entroq.LimitQueues(int(q.GetLimit())),
	}
}
