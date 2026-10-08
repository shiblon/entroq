package workgateway

import (
	"fmt"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"github.com/shiblon/entroq/pkg/pbconv"
	"google.golang.org/protobuf/encoding/protojson"
)

// Every domain object on this wire is the canonical protojson of a message in
// api/entroq.proto, carried inside a JSON envelope. A worker in any language
// generates these types from the same proto the rest of EntroQ uses and
// hand-models nothing; only the envelope -- the type tag and the turn framing --
// is this protocol's own.
//
// That is also what lets the gateway hand a client's modification straight to
// pbconv.ModifyArgsFromProto, which the gRPC service already uses: one
// validator, one arrival convention, one set of tests, rather than a second
// translation that would drift from it.
//
// MarshalJSON is on the value receiver so both direct fields and slice
// elements encode; UnmarshalJSON is on the pointer so the embedded message can
// be allocated.
type wireTask struct{ *pb.Task }

func (w wireTask) MarshalJSON() ([]byte, error) { return protojson.Marshal(w.Task) }

func (w *wireTask) UnmarshalJSON(b []byte) error {
	w.Task = new(pb.Task)
	return protojson.Unmarshal(b, w.Task)
}

type wireDoc struct{ *pb.Doc }

func (w wireDoc) MarshalJSON() ([]byte, error) { return protojson.Marshal(w.Doc) }

func (w *wireDoc) UnmarshalJSON(b []byte) error {
	w.Doc = new(pb.Doc)
	return protojson.Unmarshal(b, w.Doc)
}

type wireModReq struct{ *pb.ModifyRequest }

func (w wireModReq) MarshalJSON() ([]byte, error) { return protojson.Marshal(w.ModifyRequest) }

func (w *wireModReq) UnmarshalJSON(b []byte) error {
	w.ModifyRequest = new(pb.ModifyRequest)
	return protojson.Unmarshal(b, w.ModifyRequest)
}

type wireDep struct{ *pb.ModifyDep }

func (w wireDep) MarshalJSON() ([]byte, error) { return protojson.Marshal(w.ModifyDep) }

func (w *wireDep) UnmarshalJSON(b []byte) error {
	w.ModifyDep = new(pb.ModifyDep)
	return protojson.Unmarshal(b, w.ModifyDep)
}

type wireClaim struct{ *pb.DocSetClaim }

func (w wireClaim) MarshalJSON() ([]byte, error) { return protojson.Marshal(w.DocSetClaim) }

func (w *wireClaim) UnmarshalJSON(b []byte) error {
	w.DocSetClaim = new(pb.DocSetClaim)
	return protojson.Unmarshal(b, w.DocSetClaim)
}

// wireSet is a doc set: its lock, carried as a Doc with no ID, and its members.
// Splitting them matches pbconv, where a set's lock and its docs convert
// separately, and keeps the set's own version and length on the lock where
// entroq.DocSet has them.
type wireSet struct {
	Set  wireDoc   `json:"set"`
	Docs []wireDoc `json:"docs,omitempty"`
}

// taskToWire converts a task for the wire.
func taskToWire(t *entroq.Task) (*wireTask, error) {
	if t == nil {
		return nil, nil
	}
	pt, err := pbconv.TaskToProto(t)
	if err != nil {
		return nil, fmt.Errorf("convert task %s: %w", t.ID, err)
	}
	return &wireTask{pt}, nil
}

// setsToWire converts doc sets for the wire, each with its members.
func setsToWire(sets []*entroq.DocSet) ([]wireSet, error) {
	if len(sets) == 0 {
		return nil, nil
	}
	out := make([]wireSet, 0, len(sets))
	for _, s := range sets {
		ws := wireSet{Set: wireDoc{pbconv.DocSetToProto(s)}}
		for _, d := range s.Docs {
			pd, err := pbconv.DocToProto(d)
			if err != nil {
				return nil, fmt.Errorf("convert doc %s/%s: %w", d.Namespace, d.ID, err)
			}
			ws.Docs = append(ws.Docs, wireDoc{pd})
		}
		out = append(out, ws)
	}
	return out, nil
}

// depsToWire converts the dependencies a failed commit reported, so a client
// sees which tasks and docs went missing rather than a rendered string.
func depsToWire(depErr *entroq.DependencyError) []wireDep {
	if depErr == nil {
		return nil
	}
	details := pbconv.DependencyErrorDetails(depErr)
	out := make([]wireDep, 0, len(details))
	for _, d := range details {
		out = append(out, wireDep{d})
	}
	return out
}

// claimsFromWire turns the doc sets a client named into claim arguments.
//
// Only the set's namespace and key are read, plus whether to omit members: a
// version on the wire would suggest a client could pin which version it claims,
// and it cannot -- the worker claims whatever the set is now, as itself, until
// the task's own arrival.
func claimsFromWire(claims []wireClaim) ([]entroq.DocClaimArg, error) {
	args := make([]entroq.DocClaimArg, 0, len(claims))
	for i, c := range claims {
		id := c.GetSet()
		if id == nil {
			return nil, fmt.Errorf("doc set claim %d names no set", i)
		}
		if id.GetKey() == "" {
			return nil, fmt.Errorf("doc set claim %d names no key in namespace %q", i, id.GetNamespace())
		}
		claim := entroq.ClaimKey(id.GetNamespace(), id.GetKey())
		if c.GetOmitMembers() {
			claim = claim.WithoutMembers()
		}
		args = append(args, claim)
	}
	return args, nil
}
