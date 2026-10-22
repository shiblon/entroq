package pbconv

import (
	"fmt"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
)

// The read conversions, shared by every door a read can come through: the gRPC
// service and the work gateway both turn the same request message into the same
// client call and the same response message.
//
// They are here for the reason ModifyArgsFromProto is: a second translation
// drifts from the first. Validation lives here too, so a query is well formed
// or refused wherever it is asked rather than depending on which door it came
// through.
//
// What does NOT live here is policy. Authorization, status codes and any
// filtering that exists because of authorization stay with the caller that has
// a policy to enforce.

// MatchRequest is the shape queues and namespaces share, because they ask the
// same question of different things: match these prefixes, match these names,
// stop at this many. Both pb.QueuesRequest and pb.NamespacesRequest satisfy it.
type MatchRequest interface {
	GetMatchPrefix() []string
	GetMatchExact() []string
	GetLimit() int32
}

// MatchOptsFromProto turns a listing query into client options. A nil request
// is no filter at all, which lists everything up to the backend's own limit.
func MatchOptsFromProto(req MatchRequest) []entroq.QueuesOpt {
	if req == nil {
		return nil
	}
	return []entroq.QueuesOpt{
		entroq.MatchPrefix(req.GetMatchPrefix()...),
		entroq.MatchExact(req.GetMatchExact()...),
		entroq.LimitQueues(int(req.GetLimit())),
	}
}

// TasksQueryFromProto turns a tasks request into the queue and options a Tasks
// call takes, refusing a query that names neither a queue nor any task IDs --
// which would otherwise mean every task everywhere, never what a caller that
// omitted a field intended.
//
// ClaimedBy and WithTaskID filter only when given something, so a query naming
// just a queue is every task in it.
func TasksQueryFromProto(req *pb.TasksRequest) (string, []entroq.TasksOpt, error) {
	if req == nil {
		return "", nil, invalidf("tasks request is empty")
	}
	q := &entroq.TasksQuery{Queue: req.GetQueue(), IDs: req.GetTaskId()}
	if err := q.Validate(); err != nil {
		return "", nil, fmt.Errorf("tasks query: %w", err)
	}
	opts := []entroq.TasksOpt{
		entroq.ClaimedBy(req.GetClaimantId()),
		entroq.WithTaskID(req.GetTaskId()...),
		entroq.LimitTasks(int(req.GetLimit())),
	}
	if req.GetOmitValues() {
		opts = append(opts, entroq.OmitValues())
	}
	return req.GetQueue(), opts, nil
}

// TasksResponseFromTasks converts tasks for the wire.
func TasksResponseFromTasks(tasks []*entroq.Task) (*pb.TasksResponse, error) {
	resp := new(pb.TasksResponse)
	for _, task := range tasks {
		pt, err := TaskToProto(task)
		if err != nil {
			return nil, fmt.Errorf("task %s to proto: %w", task.ID, err)
		}
		resp.Tasks = append(resp.Tasks, pt)
	}
	return resp, nil
}

// DocQueryFromProto turns a docs request into a doc query, refusing one the
// backend could not act on. DocQuery has three mutually exclusive filter modes,
// and Validate is what knows which combinations are meant.
func DocQueryFromProto(req *pb.DocsRequest) (*entroq.DocQuery, error) {
	if req == nil {
		return nil, invalidf("docs request is empty")
	}
	q := req.GetQuery()
	dq := &entroq.DocQuery{
		Namespace:  q.GetNamespace(),
		IDs:        q.GetIds(),
		KeyExact:   q.GetKeyExact(),
		KeyStart:   q.GetKeyStart(),
		KeyEnd:     q.GetKeyEnd(),
		Limit:      int(q.GetLimit()),
		OmitValues: q.GetOmitValues(),
	}
	if err := dq.Validate(); err != nil {
		return nil, fmt.Errorf("docs query: %w", err)
	}
	return dq, nil
}

// DocsResponseFromDocs converts docs for the wire.
func DocsResponseFromDocs(docs []*entroq.Doc) (*pb.DocsResponse, error) {
	resp := new(pb.DocsResponse)
	for _, doc := range docs {
		pd, err := DocToProto(doc)
		if err != nil {
			return nil, fmt.Errorf("doc %s/%s to proto: %w", doc.Namespace, doc.ID, err)
		}
		resp.Docs = append(resp.Docs, pd)
	}
	return resp, nil
}

// QueuesResponseFromCounts converts the name-to-count form that Queues returns.
// Only NumTasks is known from it; QueuesResponseFromStats carries the rest.
func QueuesResponseFromCounts(counts map[string]int) *pb.QueuesResponse {
	resp := new(pb.QueuesResponse)
	for name, count := range counts {
		resp.Queues = append(resp.Queues, &pb.QueueStats{
			Name:     name,
			NumTasks: int32(count),
		})
	}
	return resp
}

// QueuesResponseFromStats converts the fuller stats that QueueStats returns.
func QueuesResponseFromStats(stats map[string]*entroq.QueueStat) *pb.QueuesResponse {
	resp := new(pb.QueuesResponse)
	for _, stat := range stats {
		resp.Queues = append(resp.Queues, &pb.QueueStats{
			Name:         stat.Name,
			NumTasks:     int32(stat.Size),
			NumClaimed:   int32(stat.Claimed),
			NumAvailable: int32(stat.Available),
			NumFuture:    int32(stat.Future),
			MaxClaims:    int32(stat.MaxClaims),
		})
	}
	return resp
}

// NamespacesResponseFromStats converts doc namespace stats for the wire.
func NamespacesResponseFromStats(stats map[string]*entroq.NamespaceStat) *pb.NamespacesResponse {
	resp := new(pb.NamespacesResponse)
	for _, stat := range stats {
		resp.Namespaces = append(resp.Namespaces, &pb.NamespaceStat{
			Name:       stat.Name,
			NumDocs:    int32(stat.Size),
			NumClaimed: int32(stat.Claimed),
		})
	}
	return resp
}
