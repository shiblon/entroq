package pbconv

import (
	"fmt"
	"log"
	"time"

	"github.com/shiblon/entroq"
	pb "github.com/shiblon/entroq/api"
)

// This file holds the task/doc/id conversions between the entroq domain types
// and their protobuf wire forms. The XToProto functions go domain -> proto (for
// building responses and outbound worker messages); the XFromProto functions go
// proto -> domain (for reading responses and inbound requests).

// TaskToProto converts an entroq.Task to its wire form.
func TaskToProto(t *entroq.Task) (*pb.Task, error) {
	val, err := JSONToProto(t.Value)
	if err != nil {
		return nil, fmt.Errorf("task value: %w", err)
	}
	return &pb.Task{
		Queue:      t.Queue,
		Id:         t.ID,
		Version:    t.Version,
		AtMs:       ToMS(t.At),
		ClaimantId: t.Claimant,
		Claims:     t.Claims,
		Value:      val,
		CreatedMs:  ToMS(t.Created),
		ModifiedMs: ToMS(t.Modified),
		Attempt:    t.Attempt,
		Err:        t.Err,
	}, nil
}

// TaskFromProto converts a wire Task to an entroq.Task. FromQueue is not carried
// on the wire and is left unset.
func TaskFromProto(t *pb.Task) (*entroq.Task, error) {
	val, err := ProtoToJSON(t.Value)
	if err != nil {
		return nil, fmt.Errorf("task value: %w", err)
	}
	return &entroq.Task{
		Queue:    t.Queue,
		ID:       t.Id,
		Version:  t.Version,
		At:       FromMSOrUnset(t.AtMs),
		Claimant: t.ClaimantId,
		Claims:   t.Claims,
		Value:    val,
		Created:  FromMSOrUnset(t.CreatedMs),
		Modified: FromMSOrUnset(t.ModifiedMs),
		Attempt:  t.Attempt,
		Err:      t.Err,
	}, nil
}

// TaskDataToProto converts an entroq.TaskData (an insert payload) to its wire
// form. The arrival goes out as by_ms alone: at_ms is read from a protocol-1
// client and never written, so no arrival this client asks for depends on the
// offset between its clock and the server's.
func TaskDataToProto(td *entroq.TaskData) (*pb.TaskData, error) {
	val, err := JSONToProto(td.Value)
	if err != nil {
		return nil, fmt.Errorf("task data value: %w", err)
	}
	return &pb.TaskData{
		Queue:   td.Queue,
		ByMs:    int64(td.By() / time.Millisecond),
		Value:   val,
		Attempt: td.Attempt,
		Err:     td.Err,
		Id:      td.ID,
	}, nil
}

// TaskChangeToProto converts a task into the wire TaskChange that updates it: the
// old identity (with the source queue in the ID, per the change protocol) plus
// the task's new data. With resetClaims, the change also resets the task's
// claim count, which a server that does not know change modes leaves alone.
func TaskChangeToProto(t *entroq.Task, resetClaims bool) (*pb.TaskChange, error) {
	nd, err := TaskDataToProto(t.Data())
	if err != nil {
		return nil, fmt.Errorf("change task %s: %w", t.ID, err)
	}
	pc := &pb.TaskChange{
		OldId: &pb.TaskID{
			Id:      t.ID,
			Version: t.Version,
			Queue:   t.FromQueue, // old queue goes in the ID for changes
		},
		NewData: nd,
	}
	if resetClaims {
		pc.Mode = pb.ChangeMode_CHANGE_RESET_CLAIMS
	}
	return pc, nil
}

// TaskArrivalToProto converts a task arrival to the lease-only TaskChange
// that makes it.
func TaskArrivalToProto(a *entroq.TaskArrival) *pb.TaskChange {
	return &pb.TaskChange{
		OldId:   &pb.TaskID{Id: a.ID, Version: a.Version, Queue: a.Queue},
		NewData: &pb.TaskData{ByMs: int64(a.By / time.Millisecond)},
		Mode:    pb.ChangeMode_CHANGE_LEASE,
	}
}

// DocArrivalToProto converts a doc set arrival to the lease-only DocChange
// that makes it, naming the set by its key.
func DocArrivalToProto(a *entroq.DocArrival) *pb.DocChange {
	return &pb.DocChange{
		OldId:   DocRefToProto(&a.DocID),
		NewData: &pb.DocData{ByMs: int64(a.By / time.Millisecond)},
		Mode:    pb.ChangeMode_CHANGE_LEASE,
	}
}

// DocIDToProto converts a doc's namespace, ID, and version to a wire DocID
// naming it by ID.
func DocIDToProto(ns, id string, version int32) *pb.DocID {
	return &pb.DocID{Namespace: ns, Ref: &pb.DocID_Id{Id: id}, Version: version}
}

// DocRefToProto converts a DocID to the wire, naming a set by its key when it
// is a set reference and a doc by its ID otherwise.
func DocRefToProto(did *entroq.DocID) *pb.DocID {
	if did.IsSetRef() {
		return DocSetIDToProto(did.Namespace, did.Key, did.Version)
	}
	return DocIDToProto(did.Namespace, did.ID, did.Version)
}

// DocSetIDToProto converts a doc set's namespace, key, and version to a wire
// DocID naming the set by its key.
func DocSetIDToProto(ns, key string, version int32) *pb.DocID {
	return &pb.DocID{Namespace: ns, Ref: &pb.DocID_Key{Key: key}, Version: version}
}

// TaskIDToProto converts an entroq.TaskID to the wire, carrying the queue
// because it is part of a task's identity and a backend needs it to find the
// task at all. A nil ID converts to nil, so an absent reference stays absent.
func TaskIDToProto(tid *entroq.TaskID) *pb.TaskID {
	if tid == nil {
		return nil
	}
	return &pb.TaskID{Id: tid.ID, Version: tid.Version, Queue: tid.Queue}
}

// TaskIDFromProto converts a wire TaskID to an entroq.TaskID. It cannot fail, so
// it returns no error (see the XFromProto family for the ones that can).
func TaskIDFromProto(tid *pb.TaskID) *entroq.TaskID {
	return &entroq.TaskID{
		ID:      tid.Id,
		Version: tid.Version,
		Queue:   tid.Queue,
	}
}

// DocToProto converts an entroq.Doc to its wire form.
func DocToProto(d *entroq.Doc) (*pb.Doc, error) {
	content, err := JSONToProto(d.Content)
	if err != nil {
		return nil, fmt.Errorf("doc content: %w", err)
	}
	return &pb.Doc{
		Namespace:    d.Namespace,
		Id:           d.ID,
		Version:      d.Version,
		Claimant:     d.Claimant,
		AtMs:         ToMS(d.At),
		Key:          d.Key,
		SecondaryKey: d.SecondaryKey,
		Content:      content,
		CreatedMs:    ToMS(d.Created),
		ModifiedMs:   ToMS(d.Modified),
	}, nil
}

// DocFromProto converts a wire Doc to an entroq.Doc.
func DocFromProto(d *pb.Doc) (*entroq.Doc, error) {
	content, err := ProtoToJSON(d.Content)
	if err != nil {
		return nil, fmt.Errorf("doc content: %w", err)
	}
	return &entroq.Doc{
		Namespace:    d.Namespace,
		ID:           d.Id,
		Version:      d.Version,
		Claimant:     d.Claimant,
		At:           FromMSOrUnset(d.AtMs),
		Key:          d.Key,
		SecondaryKey: d.SecondaryKey,
		Content:      content,
		Created:      FromMSOrUnset(d.CreatedMs),
		Modified:     FromMSOrUnset(d.ModifiedMs),
	}, nil
}

// DocSetToProto converts a doc set's lock to its wire form: a Doc with no
// ID, secondary key, or content. Its members travel separately.
func DocSetToProto(g *entroq.DocSet) *pb.Doc {
	return &pb.Doc{
		Namespace: g.Namespace,
		Key:       g.Key,
		Version:   g.Version,
		Claimant:  g.Claimant,
		AtMs:      ToMS(g.At),
		Len:       int32(g.NumDocs),
	}
}

// DocSetFromProto converts a wire doc set, a Doc with no ID, and its members
// to an entroq.DocSet.
func DocSetFromProto(d *pb.Doc, docs []*entroq.Doc) *entroq.DocSet {
	return &entroq.DocSet{
		Namespace: d.Namespace,
		Key:       d.Key,
		Version:   d.Version,
		Claimant:  d.Claimant,
		At:        FromMSOrUnset(d.AtMs),
		NumDocs:   int(d.Len),
		Docs:      docs,
	}
}

// IsDocSet reports whether a wire Doc stands for a doc set rather than a doc:
// sets have no ID.
func IsDocSet(d *pb.Doc) bool {
	return d.GetId() == ""
}

// MustDocFromProto is DocFromProto for callers converting a Doc whose content
// came off the wire already valid, where the re-marshal cannot realistically
// fail. If that impossible error ever does occur it is fatal: better a loud exit
// the orchestrator restarts than a doc silently returned with empty content.
func MustDocFromProto(d *pb.Doc) *entroq.Doc {
	doc, err := DocFromProto(d)
	if err != nil {
		log.Panicf("pbconv: MustDocFromProto: %v", err)
	}
	return doc
}
