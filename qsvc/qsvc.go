// Package qsvc contains the service implementation for registering with gRPC. It must be given a slice of clients to use as backends.
package qsvc

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shiblon/entroq"

	pb "github.com/shiblon/entroq/qsvc/proto"
)

type QSvc struct {
	pool chan *entroq.EntroQ
}

func New(clients []*entroq.EntroQ) *QSvc {
	svc := &QSvc{
		pool: make(chan *entroq.EntroQ, len(clients)),
	}
	for _, c := range clients {
		svc.pool <- c
	}
	return svc
}

func fromMS(ms int64) time.Time {
	return time.Unix(0, ms*int64(time.Millisecond))
}

func toMS(t time.Time) int64 {
	return t.Truncate(time.Millisecond).UnixNano() / 1000000
}

func taskToProto(t *entroq.Task) *pb.Task {
	return &pb.Task{
		Queue:      t.Queue,
		Id:         t.ID.String(),
		Version:    t.Version,
		AtMs:       toMS(t.At),
		ClaimantId: t.Claimant.String(),
		Value:      t.Value,
		CreatedMs:  toMS(t.Created),
		ModifiedMs: toMS(t.Modified),
	}
}

func (s *QSvc) getClient() *entroq.EntroQ {
	return <-s.pool
}

func (s *QSvc) returnClient(cli *entroq.EntroQ) {
	s.pool <- cli
}

func (s *QSvc) Claim(ctx context.Context, req *pb.ClaimRequest) (*pb.ClaimResponse, error) {
	client := s.getClient()
	defer s.returnClient(client)

	claimant, err := uuid.Parse(req.ClaimantId)
	if err != nil {
		return nil, fmt.Errorf("failed to parse claimant ID: %v", err)
	}
	task, err := client.Claim(ctx, claimant, req.Queue, time.Duration(req.DurationMs)*time.Millisecond)
	if err != nil {
		return &pb.ClaimResponse{
			Status: pb.Status_UNKNOWN,
			Error:  err.Error(),
		}, fmt.Errorf("failed to claim: %v", err)
	}
	return &pb.ClaimResponse{
		Status: pb.Status_OK,
		Task:   taskToProto(task),
	}, nil
}

func (s *QSvc) Modify(ctx context.Context, req *pb.ModifyRequest) (*pb.ModifyResponse, error) {
	client := s.getClient()
	defer s.returnClient(client)

	claimant, err := uuid.Parse(req.ClaimantId)
	if err != nil {
		return nil, fmt.Errorf("failed to parse claimant ID: %v", err)
	}
	var modArgs []entroq.ModifyArg
	for _, insert := range req.Inserts {
		modArgs = append(modArgs,
			entroq.InsertingInto(insert.Queue,
				entroq.WithArrivalTime(fromMS(insert.AtMs)),
				entroq.WithValue(insert.Value)))
	}
	for _, change := range req.Changes {
		id, err := uuid.Parse(change.GetOldId().Id)
		if err != nil {
			return nil, fmt.Errorf("failed to parse change id: %v", err)
		}
		t := &entroq.Task{
			ID:       id,
			Version:  change.GetOldId().Version,
			Claimant: claimant,
			Queue:    change.GetNewData().Queue,
			Value:    change.GetNewData().Value,
			At:       fromMS(change.GetNewData().AtMs),
		}
		modArgs = append(modArgs, entroq.Changing(t))
	}
	for _, delete := range req.Deletes {
		id, err := uuid.Parse(delete.Id)
		if err != nil {
			return nil, fmt.Errorf("failed to parse deletion id: %v", err)
		}
		modArgs = append(modArgs, entroq.Deleting(id, delete.Version))
	}
	for _, depend := range req.Depends {
		id, err := uuid.Parse(depend.Id)
		if err != nil {
			return nil, fmt.Errorf("failed to parse dependency id: %v", err)
		}
		modArgs = append(modArgs, entroq.DependingOn(id, depend.Version))
	}
	inserted, changed, err := client.Modify(ctx, claimant, modArgs...)
	if err != nil {
		// TODO: only return an error if there's no good information?
		return &pb.ModifyResponse{
			Status: pb.Status_UNKNOWN,
			Error:  err.Error(),
		}, fmt.Errorf("modification failed: %v", err)
	}
	resp := &pb.ModifyResponse{
		Status: pb.Status_OK,
	}
	for _, task := range inserted {
		resp.Inserted = append(resp.Inserted, taskToProto(task))
	}
	for _, task := range changed {
		resp.Changed = append(resp.Changed, taskToProto(task))
	}
	return resp, nil
}

func (s *QSvc) Tasks(ctx context.Context, req *pb.TasksRequest) (*pb.TasksResponse, error) {
	client := s.getClient()
	defer s.returnClient(client)

	claimant := uuid.Nil
	if req.ClaimantId != "" {
		var err error
		if claimant, err = uuid.Parse(req.ClaimantId); err != nil {
			return nil, fmt.Errorf("failed to parse claimant ID: %v", err)
		}
	}
	// Claimant will only really be limited if it is non-Nil.
	tasks, err := client.Tasks(ctx, req.Queue, entroq.LimitClaimant(claimant))
	if err != nil {
		return nil, fmt.Errorf("failed to get tasks: %v", err)
	}
	resp := &pb.TasksResponse{
		Status: pb.Status_OK,
	}
	for _, task := range tasks {
		resp.Tasks = append(resp.Tasks, taskToProto(task))
	}
	return resp, nil
}

func (s *QSvc) Queues(ctx context.Context, _ *pb.QueuesRequest) (*pb.QueuesResponse, error) {
	client := s.getClient()
	defer s.returnClient(client)

	queueMap, err := client.Queues(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get queues: %v", err)
	}
	resp := &pb.QueuesResponse{
		Status: pb.Status_OK,
	}
	for name, count := range queueMap {
		resp.Queues = append(resp.Queues, &pb.QueueStats{
			Name:     name,
			NumTasks: int32(count),
		})
	}
	return resp, nil
}
