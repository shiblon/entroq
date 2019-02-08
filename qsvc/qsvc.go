// Package qsvc contains the service implementation for registering with gRPC.
// This provides the service that can be registered with a grpc.Server:
//
// 	import (
// 		"context"
// 		"log"
// 		"net"
//
// 		"github.com/shiblon/entroq/pg"
// 		"github.com/shiblon/entroq/qsvc"
// 		pb "github.com/shiblon/entroq/proto"
//
// 		"google.golang.org/grpc"
// 	)
//
// 	func main() {
//		ctx := context.Background()
//
// 		listener, err := net.Listen("tcp", "localhost:54321")
// 		if err != nil {
// 			log.Fatalf("Failed to listen: %v", err)
// 		}
//
//		svc, err := qsvc.New(ctx, pg.Opener("localhost:5432", "postgres", "postgres", false))
//		if err != nil {
//			log.Fatalf("Failed to open service backends: %v", err)
//		}
//
// 		s := grpc.NewServer()
// 		pb.RegisterEntroQServer(s, svc)
// 		s.Serve(listener)
// 	}
package qsvc

import (
	"context"
	"fmt"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/google/uuid"
	"github.com/shiblon/entroq"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/shiblon/entroq/proto"
)

// QSvc is an EntroQServer.
type QSvc struct {
	impl *entroq.EntroQ
}

// New creates a new service that exposes gRPC endpoints for task queue access.
func New(ctx context.Context, opener entroq.BackendOpener) (*QSvc, error) {
	impl, err := entroq.New(ctx, opener)
	if err != nil {
		return nil, fmt.Errorf("qsvc backend client: %v", err)
	}

	return &QSvc{impl: impl}, nil
}

// Close closes the backend connections and flushes the connection free.
func (s *QSvc) Close() error {
	return s.impl.Close()
}

func fromMS(ms int64) time.Time {
	return time.Unix(0, ms*int64(time.Millisecond))
}

func toMS(t time.Time) int64 {
	return t.Truncate(time.Millisecond).UnixNano() / 1000000
}

func protoFromTask(t *entroq.Task) *pb.Task {
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

// Claim is the blocking version of TryClaim.
func (s *QSvc) Claim(ctx context.Context, req *pb.ClaimRequest) (*pb.ClaimResponse, error) {
	claimant, err := uuid.Parse(req.ClaimantId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to parse claimant ID: %v", err)
	}
	task, err := s.impl.Claim(ctx, req.Queue, time.Duration(req.DurationMs)*time.Millisecond, entroq.ClaimAs(claimant))
	if err != nil {
		if entroq.IsTimeout(err) {
			return nil, status.Errorf(codes.DeadlineExceeded, "qsvc.Claim")
		}
		return nil, status.Errorf(codes.Unknown, "qsvc.Claim: %v", err)
	}
	if task == nil {
		return new(pb.ClaimResponse), nil
	}
	return &pb.ClaimResponse{Task: protoFromTask(task)}, nil
}

// TryClaim attempts to claim a task, returning immediately. If no tasks are
// available, it returns a nil response and a nil error.
//
// If req.Wait is present, TryClaim may not return immediately, but may hold
// onto the connection until either the context expires or a task becomes
// available to claim. Callers can check for context cancelation codes to know
// that this has happened, and may opt to immediately re-send the request.
func (s *QSvc) TryClaim(ctx context.Context, req *pb.ClaimRequest) (*pb.ClaimResponse, error) {
	claimant, err := uuid.Parse(req.ClaimantId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to parse claimant ID: %v", err)
	}
	task, err := s.impl.TryClaim(ctx, req.Queue, time.Duration(req.DurationMs)*time.Millisecond, entroq.ClaimAs(claimant))
	if err != nil {
		return nil, status.Errorf(codes.Unknown, "failed to claim: %v", err)
	}
	if task == nil {
		return new(pb.ClaimResponse), nil
	}
	return &pb.ClaimResponse{Task: protoFromTask(task)}, nil
}

// Modify attempts to make the specified modification from the given
// ModifyRequest. If all goes well, it returns a ModifyResponse. If the
// modification fails due to a dependency error (one of the specified tasks was
// not present), the gRPC status mechanism is invoked to return a status with
// the details slice containing *pb.ModifyDep values. These could be used to
// reconstruct an entroq.DependencyError, or directly to find out which IDs
// caused the dependency failure. Code UNKNOWN is returned on other errors.
func (s *QSvc) Modify(ctx context.Context, req *pb.ModifyRequest) (*pb.ModifyResponse, error) {
	claimant, err := uuid.Parse(req.ClaimantId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to parse claimant ID: %v", err)
	}
	modArgs := []entroq.ModifyArg{
		entroq.ModifyAs(claimant),
	}
	for _, insert := range req.Inserts {
		modArgs = append(modArgs,
			entroq.InsertingInto(insert.Queue,
				entroq.WithArrivalTime(fromMS(insert.AtMs)),
				entroq.WithValue(insert.Value)))
	}
	for _, change := range req.Changes {
		id, err := uuid.Parse(change.GetOldId().Id)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "failed to parse change id: %v", err)
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
			return nil, status.Errorf(codes.InvalidArgument, "failed to parse deletion id: %v", err)
		}
		modArgs = append(modArgs, entroq.Deleting(id, delete.Version))
	}
	for _, depend := range req.Depends {
		id, err := uuid.Parse(depend.Id)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "failed to parse dependency id: %v", err)
		}
		modArgs = append(modArgs, entroq.DependingOn(id, depend.Version))
	}
	inserted, changed, err := s.impl.Modify(ctx, modArgs...)
	if err != nil {
		if entroq.IsDependency(err) {
			depErr := err.(entroq.DependencyError)
			tmap := map[pb.DepType][]*entroq.TaskID{
				pb.DepType_DEPEND: depErr.Depends,
				pb.DepType_DELETE: depErr.Deletes,
				pb.DepType_CHANGE: depErr.Changes,
				pb.DepType_CLAIM:  depErr.Claims,
			}

			details := []proto.Message{&pb.ModifyDep{
				Type: pb.DepType_DETAIL,
				Msg:  depErr.Message,
			}}
			for dtype, dvals := range tmap {
				for _, tid := range dvals {
					details = append(details, &pb.ModifyDep{
						Type: dtype,
						Id:   &pb.TaskID{Id: tid.ID.String(), Version: tid.Version},
					})
				}
			}

			stat, sErr := status.New(codes.NotFound, "modification dependency error").WithDetails(details...)
			if sErr != nil {
				return nil, status.Errorf(codes.NotFound, "dependency failed, and failed to add details. Both errors here: %v --- %v", err, sErr)
			}
			return nil, stat.Err()
		}
		return nil, status.Errorf(codes.Unknown, "modification failed: %v", err)
	}
	// Assemble the response.
	resp := new(pb.ModifyResponse)
	for _, task := range inserted {
		resp.Inserted = append(resp.Inserted, protoFromTask(task))
	}
	for _, task := range changed {
		resp.Changed = append(resp.Changed, protoFromTask(task))
	}
	return resp, nil
}

func (s *QSvc) Tasks(ctx context.Context, req *pb.TasksRequest) (*pb.TasksResponse, error) {
	claimant := uuid.Nil
	if req.ClaimantId != "" {
		var err error
		if claimant, err = uuid.Parse(req.ClaimantId); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "failed to parse claimant ID: %v", err)
		}
	}
	// Claimant will only really be limited if it is nonzero.
	tasks, err := s.impl.Tasks(ctx, req.Queue, entroq.LimitClaimant(claimant))
	if err != nil {
		return nil, status.Errorf(codes.Unknown, "failed to get tasks: %v", err)
	}
	resp := new(pb.TasksResponse)
	for _, task := range tasks {
		resp.Tasks = append(resp.Tasks, protoFromTask(task))
	}
	return resp, nil
}

// Queues returns a mapping from queue names to queue sizes.
func (s *QSvc) Queues(ctx context.Context, req *pb.QueuesRequest) (*pb.QueuesResponse, error) {
	queueMap, err := s.impl.Queues(ctx,
		entroq.MatchPrefix(req.MatchPrefix...),
		entroq.MatchExact(req.MatchExact...),
		entroq.LimitQueues(int(req.Limit)))
	if err != nil {
		return nil, fmt.Errorf("failed to get queues: %v", err)
	}
	resp := new(pb.QueuesResponse)
	for name, count := range queueMap {
		resp.Queues = append(resp.Queues, &pb.QueueStats{
			Name:     name,
			NumTasks: int32(count),
		})
	}
	return resp, nil
}
