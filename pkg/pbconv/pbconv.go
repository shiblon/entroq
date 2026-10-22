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
	"slices"
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

// arrivalBy reads the arrival a request asks for as a duration from the
// server's now, which is the only form the entroq API and the backends accept.
//
// by_ms is protocol 2 and is used as given. at_ms is the PROTOCOL 1 SHIM: a
// client that has no by_ms names an instant on its own clock, so the instant is
// converted here, once, against this server's clock. Doing it here rather than
// in the backends keeps the conversion in the one layer that is defined by
// protocol version, so it can be deleted outright when protocol 1 leaves
// version.ServedProtocols, and keeps every backend taking durations only.
//
// An at_ms that is unset, or farther back than entroq.ArrivalPastWindow, is no
// instruction at all and becomes zero: arrive now. Converting it literally
// would yield a duration of some two thousand years, which only works because
// something downstream caps it -- the same laundering that made FromMS wrong.
//
// Zero needs no distinguishing from absent in either field, because both mean
// now.
func arrivalBy(atMs, byMs int64, protocol int32, now time.Time, what string) (time.Duration, error) {
	at := FromMSOrUnset(atMs)
	if !at.IsZero() && protocol >= 2 {
		return 0, invalidf("%s names an arrival instant in at_ms: protocol 2 sends by_ms, a duration the server resolves on its own clock", what)
	}
	if byMs != 0 {
		return time.Duration(byMs) * time.Millisecond, nil
	}
	if at.IsZero() || at.Before(now.Add(-entroq.ArrivalPastWindow)) {
		return 0, nil
	}
	return at.Sub(now), nil
}

// FromMSOrUnset is FromMS for optional timestamps: a non-positive value means
// the field was not set and yields Go's zero time, so a backend's IsZero
// default applies. Go's zero time has no exact wire form (it encodes as a
// large negative value), and clients that omit the field send 0, which would
// otherwise decode as a real 1970 timestamp.
//
// Decode every optional timestamp with this, not with FromMS: the IsZero
// checks downstream are only correct when "unset" arrives as Go's zero time.
// Reserve FromMS for a timestamp the wire always carries, such as a server's
// own clock in a Time response.
func FromMSOrUnset(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return FromMS(ms)
}

// ToMS converts a Go time.Time to epoch milliseconds (the proto time
// representation), truncating sub-millisecond precision. Zero time encodes as
// 0, the wire's "unset" value.
func ToMS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
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

// UnsupportedRequestError marks a request this server understands but does
// not carry out yet. Callers map it onto their transport's "unimplemented"
// status.
type UnsupportedRequestError struct{ msg string }

// Error implements the error interface.
func (e *UnsupportedRequestError) Error() string { return e.msg }

func unsupportedf(format string, args ...any) *UnsupportedRequestError {
	return &UnsupportedRequestError{msg: fmt.Sprintf(format, args...)}
}

// docByID returns the ID a wire DocID names a doc by. Naming a doc by its keys
// is protocol 2, not yet carried out for anything but set leases; a DocID
// naming nothing is invalid.
func docByID(d *pb.DocID, what string) (string, error) {
	switch d.GetRef().(type) {
	case *pb.DocID_Id:
		return d.GetId(), nil
	case *pb.DocID_Key:
		return "", unsupportedf("%s naming a doc by key is not supported yet", what)
	default:
		return "", invalidf("%s names no doc", what)
	}
}

// docDepend converts a wire doc depend, which may watch one doc by ID or a whole
// set by key. Naming a set is protocol 2: it is how to depend on something the
// caller does not hold, where the set's own version is the only thing to check.
func docDepend(d *pb.DocID, protocol int32) (*entroq.DocID, error) {
	switch d.GetRef().(type) {
	case *pb.DocID_Id:
		return entroq.NewDocID(d.GetNamespace(), d.GetId(), d.GetVersion()), nil
	case *pb.DocID_Key:
		if protocol < 2 {
			return nil, invalidf("doc depend naming a set by key is protocol 2, and the request declares protocol %d", protocol)
		}
		// A key names the whole SET, never one doc in it: secondary keys are
		// not unique within a set, so no (key, secondary key) pair identifies a
		// single doc. Carrying one would suggest otherwise, so it is refused
		// rather than dropped -- name a doc by its ID.
		if sk := d.GetSecondaryKey(); sk != "" {
			return nil, invalidf("doc depend on set %q names secondary key %q: a key names the whole set, so name a doc by its ID instead", d.GetKey(), sk)
		}
		return entroq.NewDocSetRef(d.GetNamespace(), d.GetKey(), d.GetVersion()), nil
	default:
		return nil, invalidf("doc depend names no doc")
	}
}

// taskLease converts a lease-only task change, which renews or releases the
// task old names. Only the arrival time is used; a queue, if given, must be
// the task's own, and no value, attempt, error, or ID may be.
func taskLease(old *pb.TaskID, lease *pb.TaskData, protocol int32, now time.Time) (entroq.ModifyArg, error) {
	if q := lease.GetQueue(); q != "" && q != old.GetQueue() {
		return nil, invalidf("lease of task %s cannot move it: %q -> %q", old.GetId(), old.GetQueue(), q)
	}
	if lease.GetValue() != nil || lease.GetAttempt() != 0 || lease.GetErr() != "" || lease.GetId() != "" {
		return nil, invalidf("lease of task %s may set only its arrival time", old.GetId())
	}
	by, err := arrivalBy(lease.GetAtMs(), lease.GetByMs(), protocol, now, "lease of task "+old.GetId())
	if err != nil {
		return nil, err
	}
	return entroq.Arriving(entroq.ReadyIn(by).Tasks(
		&entroq.Task{ID: old.GetId(), Version: old.GetVersion(), Queue: old.GetQueue()},
	)), nil
}

// docLease converts a lease-only doc change, which renews or releases the doc
// set old names by its key. Only the arrival time is used; a namespace, key,
// or secondary key, if given, must match old, and no content may be.
func docLease(old *pb.DocID, lease *pb.DocData, protocol int32, now time.Time) (entroq.ModifyArg, error) {
	key, ok := old.GetRef().(*pb.DocID_Key)
	switch {
	case old.GetRef() == nil:
		return nil, invalidf("doc lease names no doc set")
	case !ok:
		return nil, invalidf("doc lease names a doc by ID; name its set by key")
	}
	if ns := lease.GetNamespace(); ns != "" && ns != old.GetNamespace() {
		return nil, invalidf("doc lease of %q cannot name another namespace %q", old.GetNamespace(), ns)
	}
	if k := lease.GetKey(); k != "" && k != key.Key {
		return nil, invalidf("doc lease of set %q cannot name another key %q", key.Key, k)
	}
	if sk := lease.GetSecondaryKey(); sk != "" && sk != old.GetSecondaryKey() {
		return nil, invalidf("doc lease of set %q cannot name another secondary key %q", key.Key, sk)
	}
	if lease.GetContent() != nil || lease.GetId() != "" {
		return nil, invalidf("doc lease of set %q may set only its arrival time", key.Key)
	}
	by, err := arrivalBy(lease.GetAtMs(), lease.GetByMs(), protocol, now, "lease of doc set "+key.Key)
	if err != nil {
		return nil, err
	}
	return entroq.Arriving(entroq.ReadyIn(by).Docs(
		&entroq.DocSet{Namespace: old.GetNamespace(), Key: key.Key, Version: old.GetVersion()},
	)), nil
}

// changeMode checks a change's mode against the protocol the request
// declares: modes other than CHANGE_DEFAULT are protocol 2, and a mode this
// build does not know is refused rather than read as the default.
func changeMode(mode pb.ChangeMode, protocol int32, what string) error {
	switch mode {
	case pb.ChangeMode_CHANGE_DEFAULT:
		return nil
	case pb.ChangeMode_CHANGE_RESET_CLAIMS, pb.ChangeMode_CHANGE_LEASE:
		if protocol < 2 {
			return invalidf("%s: mode %v is protocol 2, and the request declares protocol %d", what, mode, protocol)
		}
		return nil
	default:
		return invalidf("%s: unknown mode %v", what, mode)
	}
}

// ModifyArgsFromProto translates a wire ModifyRequest into the entroq modify
// arguments that apply it, read under the protocol the request declares. It is the one mapping from the language-agnostic
// protocol onto the Go modify API, which is exactly why a client (or a
// gateway-driven worker) never has to import entroq. The claimant is taken from
// the request so the applied modification is attributed to the caller.
func ModifyArgsFromProto(req *pb.ModifyRequest, protocol int32) ([]entroq.ModifyArg, error) {
	modArgs := []entroq.ModifyArg{
		entroq.ModifyAs(req.ClaimantId),
	}
	// This server's clock, used only to convert a protocol-1 client's absolute
	// arrival into the duration everything downstream speaks. See arrivalBy.
	now := entroq.ProcessTime()
	for _, insert := range req.Inserts {
		val, err := ProtoToJSON(insert.Value)
		if err != nil {
			return nil, fmt.Errorf("insert value: %w", err)
		}
		by, err := arrivalBy(insert.AtMs, insert.ByMs, protocol, now, "insert into "+insert.Queue)
		if err != nil {
			return nil, err
		}
		modArgs = append(modArgs,
			entroq.InsertingInto(insert.Queue,
				entroq.WithArrivalTimeIn(by),
				entroq.WithRawValue(val),
				entroq.WithAttempt(insert.Attempt),
				entroq.WithErr(insert.Err),
				entroq.WithID(insert.Id)))
	}
	for _, change := range req.Changes {
		if change.GetOldId() == nil {
			return nil, invalidf("task change names no task")
		}
		nd := change.GetNewData()
		if nd == nil {
			return nil, invalidf("change of task %s carries no data", change.GetOldId().GetId())
		}
		if err := changeMode(change.GetMode(), protocol, "change of task "+change.GetOldId().GetId()); err != nil {
			return nil, err
		}
		if change.GetMode() == pb.ChangeMode_CHANGE_LEASE {
			arg, err := taskLease(change.GetOldId(), nd, protocol, now)
			if err != nil {
				return nil, err
			}
			modArgs = append(modArgs, arg)
			continue
		}
		reset := change.GetMode() == pb.ChangeMode_CHANGE_RESET_CLAIMS
		val, err := ProtoToJSON(nd.GetValue())
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
		// correct FromQueue for both cases.
		oldQueue, newQueue := change.GetOldId().GetQueue(), nd.GetQueue()
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
			Attempt:  nd.GetAttempt(),
			Err:      nd.GetErr(),
		}
		var changeArgs []entroq.ChangeArg
		if newQueue != oldQueue {
			changeArgs = append(changeArgs, entroq.QueueTo(newQueue))
		}
		by, err := arrivalBy(nd.GetAtMs(), nd.GetByMs(), protocol, now, "change of task "+change.GetOldId().GetId())
		if err != nil {
			return nil, err
		}
		changeArgs = append(changeArgs, entroq.ArrivalTimeBy(by))
		if reset {
			changeArgs = append(changeArgs, entroq.ResettingClaims())
		}
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
		// Built through the options rather than as a literal, because an
		// insert names its arrival only as an unexported duration. The wire's
		// created_ms and modified_ms are deliberately not carried: they exist
		// for journal replay, which is backend-local, and a task insert has
		// never honored a client's copy of them either.
		by, err := arrivalBy(di.AtMs, di.ByMs, protocol, now, "insert into namespace "+di.Namespace)
		if err != nil {
			return nil, err
		}
		modArgs = append(modArgs, entroq.PuttingDocInto(di.Namespace,
			entroq.WithIDKeys(di.Id, di.Key, di.SecondaryKey),
			entroq.WithRawContent(val),
			entroq.WithDocArrivalTimeBy(by)))
	}
	for _, dc := range req.DocChanges {
		old := dc.GetOldId()
		if old == nil {
			return nil, invalidf("doc change names no doc")
		}
		nd := dc.GetNewData()
		if nd == nil {
			return nil, invalidf("doc change in %q carries no data", old.GetNamespace())
		}
		what := fmt.Sprintf("doc change in %q", old.GetNamespace())
		if err := changeMode(dc.GetMode(), protocol, what); err != nil {
			return nil, err
		}
		switch dc.GetMode() {
		case pb.ChangeMode_CHANGE_LEASE:
			arg, err := docLease(old, nd, protocol, now)
			if err != nil {
				return nil, err
			}
			modArgs = append(modArgs, arg)
			continue
		case pb.ChangeMode_CHANGE_RESET_CLAIMS:
			return nil, invalidf("%s: a doc has no claim count to reset", what)
		}
		id, err := docByID(old, "doc change")
		if err != nil {
			return nil, err
		}
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
			ID:           id,
			Version:      old.GetVersion(),
			Key:          nd.GetKey(),
			SecondaryKey: nd.GetSecondaryKey(),
			Content:      val,
		}
		// Pass the arrival as an option: Change releases by default, so an
		// arrival must come through the option to survive. See arrivalBy for
		// how a protocol-1 at_ms becomes the duration everything downstream
		// speaks.
		by, err := arrivalBy(nd.GetAtMs(), nd.GetByMs(), protocol, now, "change of doc "+dc.GetOldId().GetId())
		if err != nil {
			return nil, err
		}
		modArgs = append(modArgs, d.Change(entroq.WithDocArrivalTimeBy(by)))
	}
	for _, dd := range req.DocDeletes {
		id, err := docByID(dd, "doc delete")
		if err != nil {
			return nil, err
		}
		modArgs = append(modArgs, entroq.NewDocID(dd.GetNamespace(), id, dd.GetVersion()).Delete())
	}
	for _, ddep := range req.DocDepends {
		ref, err := docDepend(ddep, protocol)
		if err != nil {
			return nil, err
		}
		modArgs = append(modArgs, ref.Depend())
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
		// A task arrival is a change of arrival time on the wire; the client,
		// which knows which of its changes were arrivals, names them again.
		pb.ActionType_CHANGE: append(slices.Clone(depErr.Changes), depErr.Arrives...),
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
		// A set arrival is a change of the set on the wire, named by key.
		pb.ActionType_CHANGE: append(slices.Clone(depErr.DocChanges), depErr.DocArrives...),
		pb.ActionType_CLAIM:  depErr.DocClaims,
	}
	for dtype, dvals := range docMap {
		for _, did := range dvals {
			details = append(details, &pb.ModifyDep{Type: dtype, DocId: DocRefToProto(did)})
		}
	}
	return details
}
