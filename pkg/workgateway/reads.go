package workgateway

import (
	"context"
	"fmt"

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
func (g *Gateway) handleReadAndRespond(ctx context.Context, msg *Request) error {
	reply, err := g.read(ctx, msg)
	if err != nil {
		// A read that fails is the read failing, not the session: the client
		// hears why and decides, and the task in hand is untouched.
		reply = g.newSend(RespError, "")
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
func (g *Gateway) read(ctx context.Context, msg *Request) (*Response, error) {
	switch msg.Type {
	case ReqTime:
		t, err := g.client.Time(ctx)
		if err != nil {
			return nil, fmt.Errorf("read time: %w", err)
		}
		reply := g.newSend(RespTime, "")
		reply.TimeMs = t.UnixMilli()
		return reply, nil

	case ReqTasks:
		return g.readTasks(ctx, msg.TasksQuery)

	case ReqDocs:
		return g.readDocs(ctx, msg.DocsQuery)

	case ReqQueues:
		return g.readQueues(ctx, msg.MatchQuery)

	case ReqNamespaces:
		return g.readNamespaces(ctx, msg.MatchQuery)
	}
	return nil, fmt.Errorf("not a read: %q", msg.Type)
}

func (g *Gateway) readTasks(ctx context.Context, q *wireTasksReq) (*Response, error) {
	if q == nil {
		return nil, fmt.Errorf("a %q read carries no tasks_query", ReqTasks)
	}
	queue, opts, err := pbconv.TasksQueryFromProto(q.TasksRequest)
	if err != nil {
		return nil, err
	}
	tasks, err := g.client.Tasks(ctx, queue, opts...)
	if err != nil {
		return nil, fmt.Errorf("read tasks: %w", err)
	}
	resp, err := pbconv.TasksResponseFromTasks(tasks)
	if err != nil {
		return nil, fmt.Errorf("read tasks: %w", err)
	}
	reply := g.newSend(RespTasks, "")
	for _, pt := range resp.GetTasks() {
		reply.Tasks = append(reply.Tasks, wireTask{pt})
	}
	return reply, nil
}

func (g *Gateway) readDocs(ctx context.Context, q *wireDocsReq) (*Response, error) {
	if q == nil {
		return nil, fmt.Errorf("a %q read carries no docs_query", ReqDocs)
	}
	query, err := pbconv.DocQueryFromProto(q.DocsRequest)
	if err != nil {
		return nil, err
	}
	docs, err := g.client.Docs(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read docs: %w", err)
	}
	resp, err := pbconv.DocsResponseFromDocs(docs)
	if err != nil {
		return nil, fmt.Errorf("read docs: %w", err)
	}
	reply := g.newSend(RespDocs, "")
	for _, pd := range resp.GetDocs() {
		reply.Docs = append(reply.Docs, wireDoc{pd})
	}
	return reply, nil
}

func (g *Gateway) readQueues(ctx context.Context, q *wireMatchReq) (*Response, error) {
	counts, err := g.client.Queues(ctx, pbconv.MatchOptsFromProto(matchReq(q))...)
	if err != nil {
		return nil, fmt.Errorf("read queues: %w", err)
	}
	reply := g.newSend(RespQueues, "")
	for _, qs := range pbconv.QueuesResponseFromCounts(counts).GetQueues() {
		reply.Queues = append(reply.Queues, wireQueueStats{qs})
	}
	return reply, nil
}

func (g *Gateway) readNamespaces(ctx context.Context, q *wireMatchReq) (*Response, error) {
	stats, err := g.client.NamespaceStats(ctx, pbconv.MatchOptsFromProto(matchReq(q))...)
	if err != nil {
		return nil, fmt.Errorf("read namespaces: %w", err)
	}
	reply := g.newSend(RespNamespaces, "")
	for _, ns := range pbconv.NamespacesResponseFromStats(stats).GetNamespaces() {
		reply.Namespaces = append(reply.Namespaces, wireNamespaceStat{ns})
	}
	return reply, nil
}

// matchReq unwraps a listing query, keeping nil nil: a wrapper holding no
// message must not become a non-nil interface that then answers zero to
// everything, which would read as a filter rather than as no filter.
func matchReq(q *wireMatchReq) pbconv.MatchRequest {
	if q == nil || q.QueuesRequest == nil {
		return nil
	}
	return q.QueuesRequest
}
