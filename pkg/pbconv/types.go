package pbconv

import (
	"fmt"
	"log"

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
		At:       FromMS(t.AtMs),
		Claimant: t.ClaimantId,
		Claims:   t.Claims,
		Value:    val,
		Created:  FromMS(t.CreatedMs),
		Modified: FromMS(t.ModifiedMs),
		Attempt:  t.Attempt,
		Err:      t.Err,
	}, nil
}

// TaskDataToProto converts an entroq.TaskData (an insert payload) to its wire
// form.
func TaskDataToProto(td *entroq.TaskData) (*pb.TaskData, error) {
	val, err := JSONToProto(td.Value)
	if err != nil {
		return nil, fmt.Errorf("task data value: %w", err)
	}
	return &pb.TaskData{
		Queue:   td.Queue,
		AtMs:    ToMS(td.At),
		Value:   val,
		Attempt: td.Attempt,
		Err:     td.Err,
		Id:      td.ID,
	}, nil
}

// TaskChangeToProto converts a task into the wire TaskChange that updates it: the
// old identity (with the source queue in the ID, per the change protocol) plus
// the task's new data.
func TaskChangeToProto(t *entroq.Task) (*pb.TaskChange, error) {
	nd, err := TaskDataToProto(t.Data())
	if err != nil {
		return nil, fmt.Errorf("change task %s: %w", t.ID, err)
	}
	return &pb.TaskChange{
		OldId: &pb.TaskID{
			Id:      t.ID,
			Version: t.Version,
			Queue:   t.FromQueue, // old queue goes in the ID for changes
		},
		NewData: nd,
	}, nil
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
		At:           FromMS(d.AtMs),
		Key:          d.Key,
		SecondaryKey: d.SecondaryKey,
		Content:      content,
		Created:      FromMS(d.CreatedMs),
		Modified:     FromMS(d.ModifiedMs),
	}, nil
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
