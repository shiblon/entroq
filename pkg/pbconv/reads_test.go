package pbconv

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
)

// applyMatch applies listing options the way the client does, so a test checks
// what the query BECOMES rather than how many options were produced.
func applyMatch(opts []entroq.QueuesOpt) *entroq.MatchQuery {
	q := new(entroq.MatchQuery)
	for _, opt := range opts {
		opt(q)
	}
	return q
}

// applyTasks applies task options. The client argument is only read by the
// options that filter on the client's own identity, which none of these are.
func applyTasks(opts []entroq.TasksOpt) *entroq.TasksQuery {
	q := new(entroq.TasksQuery)
	for _, opt := range opts {
		opt(nil, q)
	}
	return q
}

// TestMatchOptsFromProtoServesBothListings is the claim MatchRequest makes:
// queues and namespaces ask the same question of different things, so one
// conversion serves both. A compile failure here is the point as much as a
// test failure.
func TestMatchOptsFromProtoServesBothListings(t *testing.T) {
	var (
		_ MatchRequest = (*pb.QueuesRequest)(nil)
		_ MatchRequest = (*pb.NamespacesRequest)(nil)
	)

	for name, req := range map[string]MatchRequest{
		"queues":     &pb.QueuesRequest{MatchPrefix: []string{"in/"}, MatchExact: []string{"other"}, Limit: 7},
		"namespaces": &pb.NamespacesRequest{MatchPrefix: []string{"in/"}, MatchExact: []string{"other"}, Limit: 7},
	} {
		t.Run(name, func(t *testing.T) {
			q := applyMatch(MatchOptsFromProto(req))
			if !slices.Equal(q.MatchPrefix, []string{"in/"}) {
				t.Errorf("MatchPrefix = %v, want [in/]", q.MatchPrefix)
			}
			if !slices.Equal(q.MatchExact, []string{"other"}) {
				t.Errorf("MatchExact = %v, want [other]", q.MatchExact)
			}
			if q.Limit != 7 {
				t.Errorf("Limit = %d, want 7", q.Limit)
			}
		})
	}
}

// TestMatchOptsFromProtoNilIsNoFilter checks the distinction that matters: no
// query means list everything, NOT match nothing. A nil request producing
// options that filtered on empty strings would silently return an empty
// listing, which looks like an empty EntroQ.
func TestMatchOptsFromProtoNilIsNoFilter(t *testing.T) {
	if opts := MatchOptsFromProto(nil); opts != nil {
		q := applyMatch(opts)
		t.Errorf("a nil request produced %d options (%+v); want none, so nothing is filtered", len(opts), q)
	}
	// An empty request is also no filter: the fields are simply unset.
	q := applyMatch(MatchOptsFromProto(&pb.QueuesRequest{}))
	if len(q.MatchPrefix) != 0 || len(q.MatchExact) != 0 || q.Limit != 0 {
		t.Errorf("an empty request produced a filter: %+v", q)
	}
}

// TestTasksQueryFromProtoRefusesTooBroad is the refusal worth having: a query
// naming neither a queue nor any task IDs would mean every task everywhere,
// which is never what a caller that omitted a field intended.
func TestTasksQueryFromProtoRefusesTooBroad(t *testing.T) {
	for name, req := range map[string]*pb.TasksRequest{
		"nothing at all":  {},
		"a limit only":    {Limit: 10},
		"a claimant only": {ClaimantId: "someone"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := TasksQueryFromProto(req); err == nil {
				t.Errorf("TasksQueryFromProto accepted a query naming %s", name)
			}
		})
	}
	t.Run("a nil request", func(t *testing.T) {
		if _, _, err := TasksQueryFromProto(nil); err == nil {
			t.Error("TasksQueryFromProto accepted a nil request")
		}
	})
	t.Run("task IDs alone are enough", func(t *testing.T) {
		if _, _, err := TasksQueryFromProto(&pb.TasksRequest{TaskId: []string{"abc"}}); err != nil {
			t.Errorf("a query naming task IDs was refused: %v", err)
		}
	})
}

// TestTasksQueryFromProtoBuildsTheQuery checks that what a caller asked for is
// what the client is told, including the queue, which comes back separately
// because Tasks takes it as its own argument.
func TestTasksQueryFromProtoBuildsTheQuery(t *testing.T) {
	queue, opts, err := TasksQueryFromProto(&pb.TasksRequest{
		Queue:      "inbox",
		TaskId:     []string{"a", "b"},
		ClaimantId: "someone",
		Limit:      3,
		OmitValues: true,
	})
	if err != nil {
		t.Fatalf("TasksQueryFromProto: %v", err)
	}
	if queue != "inbox" {
		t.Errorf("queue = %q, want inbox", queue)
	}
	q := applyTasks(opts)
	if !slices.Equal(q.IDs, []string{"a", "b"}) {
		t.Errorf("IDs = %v, want [a b]", q.IDs)
	}
	if q.Claimant != "someone" {
		t.Errorf("Claimant = %q, want someone", q.Claimant)
	}
	if q.Limit != 3 {
		t.Errorf("Limit = %d, want 3", q.Limit)
	}
	if !q.OmitValues {
		t.Error("OmitValues = false, want true")
	}
}

// TestDocQueryFromProtoRefusesAndConverts covers the other validated query.
// DocQuery has three mutually exclusive filter modes, and Validate is what
// knows which combinations are meant, so the conversion must ask it.
func TestDocQueryFromProtoRefusesAndConverts(t *testing.T) {
	if _, err := DocQueryFromProto(nil); err == nil {
		t.Error("DocQueryFromProto accepted a nil request")
	}
	if _, err := DocQueryFromProto(&pb.DocsRequest{Query: &pb.DocQuery{}}); err == nil {
		t.Error("DocQueryFromProto accepted a query naming no namespace")
	}
	// Two filter modes at once is NOT refused: DocQuery.Validate checks only
	// the namespace, and every backend resolves the ambiguity the same way, by
	// letting IDs win. Asserted so that a backend which stopped agreeing, or a
	// Validate that started refusing, is a deliberate change and not a
	// surprise here.
	both, err := DocQueryFromProto(&pb.DocsRequest{Query: &pb.DocQuery{
		Namespace: "cfg",
		Ids:       []string{"doc-1"},
		KeyExact:  "limits",
	}})
	if err != nil {
		t.Errorf("a query naming two filter modes was refused: %v", err)
	} else if len(both.IDs) != 1 || both.KeyExact != "limits" {
		t.Errorf("converted query = %+v; the conversion must pass both through and leave precedence to the backend", both)
	}

	dq, err := DocQueryFromProto(&pb.DocsRequest{Query: &pb.DocQuery{
		Namespace:  "cfg",
		KeyStart:   "a",
		KeyEnd:     "m",
		Limit:      4,
		OmitValues: true,
	}})
	if err != nil {
		t.Fatalf("DocQueryFromProto: %v", err)
	}
	if dq.Namespace != "cfg" || dq.KeyStart != "a" || dq.KeyEnd != "m" || dq.Limit != 4 || !dq.OmitValues {
		t.Errorf("converted query = %+v, want the one that was asked for", dq)
	}
}

// TestQueuesResponseKeepsWhatItKnows checks that the two queue listings differ
// in exactly the way their sources differ: Queues knows a count, QueueStats
// knows the breakdown.
func TestQueuesResponseKeepsWhatItKnows(t *testing.T) {
	t.Run("from counts", func(t *testing.T) {
		resp := QueuesResponseFromCounts(map[string]int{"inbox": 2})
		if len(resp.GetQueues()) != 1 {
			t.Fatalf("got %d queues, want 1", len(resp.GetQueues()))
		}
		q := resp.GetQueues()[0]
		if q.GetName() != "inbox" || q.GetNumTasks() != 2 {
			t.Errorf("queue = %+v, want inbox with 2 tasks", q)
		}
		// A count says nothing about the breakdown, and must not pretend to.
		if q.GetNumClaimed() != 0 || q.GetNumAvailable() != 0 {
			t.Errorf("a count-only listing invented a breakdown: %+v", q)
		}
	})

	t.Run("from stats", func(t *testing.T) {
		resp := QueuesResponseFromStats(map[string]*entroq.QueueStat{
			"inbox": {Name: "inbox", Size: 5, Claimed: 2, Available: 3, Future: 1, MaxClaims: 4},
		})
		if len(resp.GetQueues()) != 1 {
			t.Fatalf("got %d queues, want 1", len(resp.GetQueues()))
		}
		q := resp.GetQueues()[0]
		if q.GetNumTasks() != 5 || q.GetNumClaimed() != 2 || q.GetNumAvailable() != 3 ||
			q.GetNumFuture() != 1 || q.GetMaxClaims() != 4 {
			t.Errorf("queue = %+v, want the whole breakdown carried over", q)
		}
	})

	t.Run("an empty listing is empty, not nil-hostile", func(t *testing.T) {
		if got := len(QueuesResponseFromCounts(nil).GetQueues()); got != 0 {
			t.Errorf("got %d queues from nothing, want 0", got)
		}
		if got := len(NamespacesResponseFromStats(nil).GetNamespaces()); got != 0 {
			t.Errorf("got %d namespaces from nothing, want 0", got)
		}
	})
}

// TestNamespacesResponseFromStats checks the doc-side listing, whose two
// numbers are the ones a doc namespace actually has.
func TestNamespacesResponseFromStats(t *testing.T) {
	resp := NamespacesResponseFromStats(map[string]*entroq.NamespaceStat{
		"cfg": {Name: "cfg", Size: 3, Claimed: 1},
	})
	if len(resp.GetNamespaces()) != 1 {
		t.Fatalf("got %d namespaces, want 1", len(resp.GetNamespaces()))
	}
	ns := resp.GetNamespaces()[0]
	if ns.GetName() != "cfg" || ns.GetNumDocs() != 3 || ns.GetNumClaimed() != 1 {
		t.Errorf("namespace = %+v, want cfg with 3 docs and 1 claimed", ns)
	}
}

// TestResponsesFromDomainObjects checks the two converters that can fail, and
// that a value crosses as JSON rather than as the base64 a Go struct would
// have produced.
func TestResponsesFromDomainObjects(t *testing.T) {
	resp, err := TasksResponseFromTasks([]*entroq.Task{
		{Queue: "inbox", ID: "11111111-1111-1111-1111-111111111111", Value: json.RawMessage(`{"n":1}`)},
	})
	if err != nil {
		t.Fatalf("TasksResponseFromTasks: %v", err)
	}
	if len(resp.GetTasks()) != 1 {
		t.Fatalf("got %d tasks, want 1", len(resp.GetTasks()))
	}
	if got := resp.GetTasks()[0].GetValue().GetStructValue().GetFields()["n"].GetNumberValue(); got != 1 {
		t.Errorf("task value n = %v, want 1: the value did not cross as JSON", got)
	}

	docs, err := DocsResponseFromDocs([]*entroq.Doc{
		{Namespace: "cfg", ID: "doc-1", Key: "limits", Content: json.RawMessage(`{"max":5}`)},
	})
	if err != nil {
		t.Fatalf("DocsResponseFromDocs: %v", err)
	}
	if len(docs.GetDocs()) != 1 {
		t.Fatalf("got %d docs, want 1", len(docs.GetDocs()))
	}
	if got := docs.GetDocs()[0].GetKey(); got != "limits" {
		t.Errorf("doc key = %q, want limits", got)
	}
}
