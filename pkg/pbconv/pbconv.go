// Package pbconv is the single place that translates between the EntroQ wire
// protocol (the protobuf messages in package api) and the entroq domain types.
// It exists so the gRPC client (eqgrpc), the gRPC service (eqsvcgrpc), and the
// work gateway (workgateway, which speaks the same protobuf messages as
// newline-delimited or WebSocket JSON) all share one conversion rather than each
// re-deriving it. The proto is the schema; this package tracks it, so it lives
// beside neither transport but is imported by all of them.
//
// It is deliberately transport-neutral: errors are plain errors (or the typed
// InvalidRequestError for caller-fixable requests), and callers map them onto
// their own transport's status vocabulary. It never imports gRPC.
package pbconv

import (
	"fmt"
	"time"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
	"google.golang.org/protobuf/types/known/structpb"
)

// FromMS converts epoch milliseconds (the proto time representation) to a Go
// time.Time.
func FromMS(ms int64) time.Time {
	return time.Unix(0, ms*int64(time.Millisecond))
}

// ToMS converts a Go time.Time to epoch milliseconds (the proto time
// representation), truncating sub-millisecond precision.
func ToMS(t time.Time) int64 {
	return t.Truncate(time.Millisecond).UnixNano() / 1000000
}

// JSONToProto converts a raw JSON value into a structpb.Value for the wire. A
// nil input means "no value" and yields a nil Value (an unset proto field that
// ProtoToJSON round-trips back to nil); an empty but non-nil input is JSON null.
func JSONToProto(raw []byte) (*structpb.Value, error) {
	if raw == nil {
		return nil, nil
	}
	if len(raw) == 0 {
		return structpb.NewNullValue(), nil
	}
	v := new(structpb.Value)
	if err := v.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("json to proto: %w", err)
	}
	return v, nil
}

// ProtoToJSON converts a wire structpb.Value back into raw JSON bytes. A nil
// Value round-trips to nil ("no value").
func ProtoToJSON(v *structpb.Value) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	b, err := v.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("proto to json: %w", err)
	}
	return b, nil
}

// InvalidRequestError marks a request that is malformed in a caller-fixable way
// (for example a doc change that tries to move namespaces), as distinct from an
// internal translation failure. Callers map it onto their transport's
// "invalid argument" status; over the work gateway it is a client protocol bug.
type InvalidRequestError struct{ msg string }

// Error implements the error interface.
func (e *InvalidRequestError) Error() string { return e.msg }

func invalidf(format string, args ...any) *InvalidRequestError {
	return &InvalidRequestError{msg: fmt.Sprintf(format, args...)}
}

// ModifyArgsFromProto translates a wire ModifyRequest into the entroq modify
// arguments that apply it. It is the one mapping from the language-agnostic
// protocol onto the Go modify API, which is exactly why a client (or a
// gateway-driven worker) never has to import entroq. The claimant is taken from
// the request so the applied modification is attributed to the caller.
func ModifyArgsFromProto(req *pb.ModifyRequest) ([]entroq.ModifyArg, error) {
	modArgs := []entroq.ModifyArg{
		entroq.ModifyAs(req.ClaimantId),
	}
	for _, insert := range req.Inserts {
		val, err := ProtoToJSON(insert.Value)
		if err != nil {
			return nil, fmt.Errorf("insert value: %w", err)
		}
		modArgs = append(modArgs,
			entroq.InsertingInto(insert.Queue,
				entroq.WithArrivalTime(FromMS(insert.AtMs)),
				entroq.WithRawValue(val),
				entroq.WithAttempt(insert.Attempt),
				entroq.WithErr(insert.Err),
				entroq.WithID(insert.Id)))
	}
	for _, change := range req.Changes {
		val, err := ProtoToJSON(change.GetNewData().GetValue())
		if err != nil {
			return nil, fmt.Errorf("change value: %w", err)
		}
		// The queue is part of the modify key: the backend binds a change to the
		// task's CURRENT queue (see eqmem's queue-integrity check), so we build the
		// task in that queue and let QueueTo move it. The wire splits the two
		// queues into OldId.Queue (the source, i.e. current) and NewData.Queue (the
		// destination). Task.Change always derives FromQueue from the task's Queue
		// field, so if we set Queue to the destination up front, every move would
		// report its source as its own target and fail the integrity check. Setting
		// Queue to the source and applying QueueTo only on an actual move yields the
		// correct FromQueue for both cases. Nil-safe getters throughout: a change
		// with OldId/NewData unset yields empty fields and fails the integrity check
		// downstream rather than panicking.
		oldQueue, newQueue := change.GetOldId().GetQueue(), change.GetNewData().GetQueue()
		// An empty destination means "no move": normalize it to the current queue
		// so a plain change stays put, and only a different, non-empty destination
		// moves the task.
		if newQueue == "" {
			newQueue = oldQueue
		}
		t := &entroq.Task{
			ID:       change.GetOldId().GetId(),
			Version:  change.GetOldId().GetVersion(),
			Claimant: req.ClaimantId,
			Queue:    oldQueue, // current queue; Task.Change derives FromQueue from it
			Value:    val,
			Attempt:  change.GetNewData().GetAttempt(),
			Err:      change.GetNewData().GetErr(),
		}
		var changeArgs []entroq.ChangeArg
		if newQueue != oldQueue {
			changeArgs = append(changeArgs, entroq.QueueTo(newQueue))
		}
		changeArgs = append(changeArgs, entroq.ArrivalTimeTo(FromMS(change.GetNewData().GetAtMs())))
		modArgs = append(modArgs, t.Change(changeArgs...))
	}
	for _, del := range req.Deletes {
		modArgs = append(modArgs, entroq.NewTaskID(del.Id, del.Version, del.Queue).Delete())
	}
	for _, dep := range req.Depends {
		modArgs = append(modArgs, entroq.NewTaskID(dep.Id, dep.Version, dep.Queue).Depend())
	}
	for _, di := range req.DocInserts {
		val, err := ProtoToJSON(di.Content)
		if err != nil {
			return nil, fmt.Errorf("doc insert content: %w", err)
		}
		modArgs = append(modArgs, entroq.PuttingDoc(&entroq.DocData{
			Namespace:    di.Namespace,
			ID:           di.Id,
			Key:          di.Key,
			SecondaryKey: di.SecondaryKey,
			Content:      val,
			Created:      FromMS(di.CreatedMs),
			Modified:     FromMS(di.ModifiedMs),
		}))
	}
	for _, dc := range req.DocChanges {
		old := dc.GetOldId()
		nd := dc.GetNewData()
		// Docs do not move between namespaces: a change is always in place. A
		// non-empty destination namespace that differs from the source is rejected
		// rather than silently applied in the source namespace. (An empty
		// destination namespace means "unspecified", so the source is used.)
		if to := nd.GetNamespace(); to != "" && to != old.GetNamespace() {
			return nil, invalidf("doc change cannot move namespaces: %q -> %q", old.GetNamespace(), to)
		}
		val, err := ProtoToJSON(nd.Content)
		if err != nil {
			return nil, fmt.Errorf("doc change content: %w", err)
		}
		d := &entroq.Doc{
			Namespace:    old.GetNamespace(),
			ID:           old.GetId(),
			Version:      old.GetVersion(),
			Key:          nd.GetKey(),
			SecondaryKey: nd.GetSecondaryKey(),
			Content:      val,
		}
		// Pass the wire arrival time as an option: Change resets At by default
		// (release), so an explicit time must come through the option to survive. A
		// far-past value (an unset wire time) is capped to now by the backend.
		modArgs = append(modArgs, d.Change(entroq.WithDocArrivalTime(FromMS(nd.GetAtMs()))))
	}
	for _, dd := range req.DocDeletes {
		modArgs = append(modArgs, entroq.NewDocID(dd.Namespace, dd.Id, dd.Version).Delete())
	}
	for _, ddep := range req.DocDepends {
		modArgs = append(modArgs, entroq.NewDocID(ddep.Namespace, ddep.Id, ddep.Version).Depend())
	}
	return modArgs, nil
}

// DependencyErrorDetails renders a DependencyError as the wire ModifyDep list: a
// leading DETAIL entry carrying the human-readable message, then one entry per
// failed task and doc dependency, tagged with the operation that failed. The
// gRPC service attaches these as status details; the work gateway sends them to
// the worker so a language-agnostic client can inspect exactly which
// dependencies failed, the same way a Go worker reads the DependencyError.
func DependencyErrorDetails(depErr *entroq.DependencyError) []*pb.ModifyDep {
	details := []*pb.ModifyDep{{
		Type: pb.ActionType_DETAIL,
		Msg:  depErr.Message,
	}}
	taskMap := map[pb.ActionType][]*entroq.TaskID{
		pb.ActionType_INSERT: depErr.Inserts,
		pb.ActionType_DEPEND: depErr.Depends,
		pb.ActionType_DELETE: depErr.Deletes,
		pb.ActionType_CHANGE: depErr.Changes,
		pb.ActionType_CLAIM:  depErr.Claims,
	}
	for dtype, dvals := range taskMap {
		for _, tid := range dvals {
			details = append(details, &pb.ModifyDep{
				Type: dtype,
				Id:   &pb.TaskID{Id: tid.ID, Version: tid.Version, Queue: tid.Queue},
			})
		}
	}
	docMap := map[pb.ActionType][]*entroq.DocID{
		pb.ActionType_INSERT: depErr.DocInserts,
		pb.ActionType_DELETE: depErr.DocDeletes,
		pb.ActionType_DEPEND: depErr.DocDepends,
		pb.ActionType_CHANGE: depErr.DocChanges,
		pb.ActionType_CLAIM:  depErr.DocClaims,
	}
	for dtype, dvals := range docMap {
		for _, did := range dvals {
			details = append(details, &pb.ModifyDep{
				Type:  dtype,
				DocId: &pb.DocID{Namespace: did.Namespace, Id: did.ID, Version: did.Version},
			})
		}
	}
	return details
}
