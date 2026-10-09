package workgateway

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestWireTaskIsProtojson is the promise this package makes to a worker in
// another language: every domain object on the wire is the canonical protojson
// of a message in api/entroq.proto, so the worker generates its types from that
// proto and hand-models nothing.
//
// Encoding an entroq.Task directly would have produced Go field names and a
// base64 value, which no generated client could read.
func TestWireTaskIsProtojson(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	task := &entroq.Task{
		Queue:    "inbox",
		ID:       "11111111-1111-1111-1111-111111111111",
		Version:  7,
		At:       now.Add(time.Minute),
		Claimant: "workgateway/abc",
		Claims:   3,
		Value:    json.RawMessage(`{"n":42}`),
		Created:  now,
		Modified: now,
	}

	wt, err := taskToWire(task)
	if err != nil {
		t.Fatalf("taskToWire: %v", err)
	}
	raw, err := json.Marshal(struct {
		Task *wireTask `json:"task"`
	}{wt})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Proto JSON names, not Go ones. atMs is the giveaway: an entroq.Task
	// would have written "at" as an RFC3339 string.
	for _, want := range []string{`"queue":"inbox"`, `"atMs"`, `"claimantId"`, `"value":{"n":42}`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("encoded task is missing %s:\n%s", want, raw)
		}
	}

	// And it decodes as the proto a generated client would use.
	var back struct {
		Task *wireTask `json:"task"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := back.Task.GetQueue(); got != task.Queue {
		t.Errorf("queue = %q, want %q", got, task.Queue)
	}
	if got := back.Task.GetVersion(); got != task.Version {
		t.Errorf("version = %d, want %d", got, task.Version)
	}
	if got := back.Task.GetClaims(); got != task.Claims {
		t.Errorf("claims = %d, want %d", got, task.Claims)
	}
}

// TestWireSetsCarryLockAndMembers checks that a doc set crosses as its lock
// plus its members, with the set's own version on the lock, so a client can
// tell an empty set from a set it does not hold.
func TestWireSetsCarryLockAndMembers(t *testing.T) {
	sets := []*entroq.DocSet{
		{
			Namespace: "orders",
			Key:       "cust-1",
			Version:   4,
			Claimant:  "workgateway/abc",
			NumDocs:   1,
			Docs: []*entroq.Doc{
				{Namespace: "orders", ID: "doc-1", Version: 2, Key: "cust-1", Content: json.RawMessage(`{"total":9}`)},
			},
		},
		// A set with no docs at all: claimed, empty, and that is not an error.
		{Namespace: "stock", Key: "sku-9", Version: 1, Claimant: "workgateway/abc"},
	}

	ws, err := setsToWire(sets)
	if err != nil {
		t.Fatalf("setsToWire: %v", err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d wire sets, want 2", len(ws))
	}
	if got := ws[0].Set.GetVersion(); got != 4 {
		t.Errorf("first set version = %d, want 4", got)
	}
	if len(ws[0].Docs) != 1 {
		t.Errorf("first set has %d docs, want 1", len(ws[0].Docs))
	}
	if len(ws[1].Docs) != 0 {
		t.Errorf("empty set has %d docs, want none", len(ws[1].Docs))
	}

	// It survives the envelope.
	raw, err := json.Marshal(ws)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back []wireSet
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(back) != 2 || back[0].Set.GetKey() != "cust-1" || len(back[0].Docs) != 1 {
		t.Errorf("round trip lost the set:\n%s", raw)
	}
}

// TestModifyArgsRefusesAnArrivalInstant is the rule made enforceable rather
// than documented: an arrival is an INSTRUCTION, so it crosses as a duration a
// server resolves on its own clock, never as an instant from a client's.
//
// The gateway gets this from pbconv.ModifyArgsFromProto by passing
// version.Protocol, which is the whole reason the modification is a proto on
// the wire instead of something this package translates itself.
func TestModifyArgsRefusesAnArrivalInstant(t *testing.T) {
	g := &Gateway{}
	mod := &wireModReq{&pb.ModifyRequest{
		Inserts: []*pb.TaskData{{Queue: "q", AtMs: time.Now().Add(time.Minute).UnixMilli()}},
	}}
	if _, err := g.modifyArgs(mod); err == nil {
		t.Error("an insert naming an arrival instant was accepted, want a refusal")
	}

	// The same insert, said as a duration, is fine.
	ok := &wireModReq{&pb.ModifyRequest{
		Inserts: []*pb.TaskData{{Queue: "q", ByMs: time.Minute.Milliseconds()}},
	}}
	if _, err := g.modifyArgs(ok); err != nil {
		t.Errorf("an insert naming an arrival duration was refused: %v", err)
	}
}

// TestModifyArgsRefusesAClientClaimant checks that a client naming a claimant
// is told it is wrong rather than silently overridden. The run's client appends
// its own claimant last and would win regardless, so this exists to correct a
// client's understanding, not to protect the commit.
func TestModifyArgsRefusesAClientClaimant(t *testing.T) {
	g := &Gateway{}
	mod := &wireModReq{&pb.ModifyRequest{ClaimantId: "someone-else"}}
	_, err := g.modifyArgs(mod)
	if err == nil {
		t.Fatal("a modification naming a claimant was accepted, want a refusal")
	}
	if !strings.Contains(err.Error(), "someone-else") {
		t.Errorf("refusal does not name the offending claimant: %v", err)
	}
}

// TestModifyArgsEmptyIsNoModification checks that a client with nothing to
// commit says so by sending nothing, which leaves the task's own disposition
// to stand alone.
func TestModifyArgsEmptyIsNoModification(t *testing.T) {
	g := &Gateway{}
	for _, mod := range []*wireModReq{nil, {}} {
		args, err := g.modifyArgs(mod)
		if err != nil {
			t.Errorf("modifyArgs(%v): %v", mod, err)
		}
		if len(args) != 0 {
			t.Errorf("modifyArgs(%v) produced %d args, want none", mod, len(args))
		}
	}
}

// TestClaimsFromWire checks what a client may say about the doc sets it needs,
// and what it may not. Only namespace, key and omit-members are read: a version
// would suggest a client could pin which version it claims, and it cannot.
func TestClaimsFromWire(t *testing.T) {
	claims := []wireClaim{
		{&pb.DocSetClaim{Set: &pb.DocID{Namespace: "orders", Ref: &pb.DocID_Key{Key: "cust-1"}}}},
		{&pb.DocSetClaim{Set: &pb.DocID{Namespace: "stock", Ref: &pb.DocID_Key{Key: "sku-9"}}, OmitMembers: true}},
	}
	args, err := claimsFromWire(claims)
	if err != nil {
		t.Fatalf("claimsFromWire: %v", err)
	}
	claim := entroq.NewDocClaim(args...)
	if len(claim.Sets) != 2 {
		t.Fatalf("got %d sets, want 2", len(claim.Sets))
	}
	if claim.Sets[0].Namespace != "orders" || claim.Sets[0].Key != "cust-1" {
		t.Errorf("first set = %+v, want orders/cust-1", claim.Sets[0])
	}
	if claim.Sets[0].OmitMembers {
		t.Error("first set omits members, but the client did not ask it to")
	}
	if !claim.Sets[1].OmitMembers {
		t.Error("second set includes members, but the client asked to omit them")
	}
}

// TestClaimsFromWireRefusesNonsense checks that a claim the gateway cannot act
// on is named, not guessed at: a set with no key would otherwise claim the
// empty key in some namespace, which is a real set someone else may hold.
func TestClaimsFromWireRefusesNonsense(t *testing.T) {
	for name, claims := range map[string][]wireClaim{
		"no set": {{&pb.DocSetClaim{}}},
		"no key": {{&pb.DocSetClaim{Set: &pb.DocID{Namespace: "orders"}}}},
		"an id instead of a key": {{&pb.DocSetClaim{
			Set: &pb.DocID{Namespace: "orders", Ref: &pb.DocID_Id{Id: "doc-1"}},
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := claimsFromWire(claims); err == nil {
				t.Errorf("claimsFromWire accepted a claim with %s", name)
			}
		})
	}
}

// TestDepsToWireNamesWhatMoved checks that a client learns which tasks and docs
// a failed commit depended on, rather than being handed a rendered string to
// parse.
func TestDepsToWireNamesWhatMoved(t *testing.T) {
	if got := depsToWire(nil); got != nil {
		t.Errorf("depsToWire(nil) = %v, want nil", got)
	}

	depErr := &entroq.DependencyError{
		Message: "task is gone",
		Depends: []*entroq.TaskID{{ID: "22222222-2222-2222-2222-222222222222", Version: 3}},
	}
	deps := depsToWire(depErr)

	// A leading DETAIL entry carries the message, then one entry per failed
	// dependency. The message rides in the list rather than beside it, so a
	// client has one thing to read and never has to parse prose.
	if len(deps) != 2 {
		t.Fatalf("got %d deps, want a DETAIL entry plus the one failed dependency", len(deps))
	}
	if got := deps[0].GetType(); got != pb.ActionType_DETAIL {
		t.Errorf("first dep type = %v, want %v", got, pb.ActionType_DETAIL)
	}
	if got := deps[0].GetMsg(); got != depErr.Message {
		t.Errorf("DETAIL message = %q, want %q", got, depErr.Message)
	}
	if got := deps[1].GetType(); got != pb.ActionType_DEPEND {
		t.Errorf("second dep type = %v, want %v", got, pb.ActionType_DEPEND)
	}
	raw, err := protojson.Marshal(deps[1].ModifyDep)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), "22222222-2222-2222-2222-222222222222") {
		t.Errorf("dep does not name the task it depended on:\n%s", raw)
	}
}

// TestModifyArgsRequiresAQueue is the error a wire client gets most often, and
// the reason it is worth catching here: a TaskID looks complete with an id and
// a version, and the backend's answer for the omission reads like a WRONG
// queue rather than a missing one.
func TestModifyArgsRequiresAQueue(t *testing.T) {
	g := &Gateway{}
	for name, mod := range map[string]*pb.ModifyRequest{
		"delete": {Deletes: []*pb.TaskID{{Id: "task-1", Version: 1}}},
		"depend": {Depends: []*pb.TaskID{{Id: "task-1", Version: 1}}},
		"insert": {Inserts: []*pb.TaskData{{}}},
		"change": {Changes: []*pb.TaskChange{{
			OldId:   &pb.TaskID{Id: "task-1", Version: 1},
			NewData: &pb.TaskData{Queue: "inbox"},
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := g.modifyArgs(&wireModReq{mod})
			if err == nil {
				t.Fatalf("a %s naming no queue was accepted", name)
			}
			if !strings.Contains(err.Error(), "no queue") {
				t.Errorf("error does not say what is missing: %v", err)
			}
		})
	}

	// Naming it is all that is asked.
	ok := &pb.ModifyRequest{
		Deletes: []*pb.TaskID{{Queue: "inbox", Id: "task-1", Version: 1}},
	}
	if _, err := g.modifyArgs(&wireModReq{ok}); err != nil {
		t.Errorf("a delete naming its queue was refused: %v", err)
	}
}
