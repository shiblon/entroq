// Package grpc provides a gRPC backend for EntroQ. This is the backend that is
// commonly used by clients of an EntroQ task service, set up thus:
//
// 	Server:
// 		qsvc -> entroq library -> some backend (e.g., pg)
//
// 	Client:
// 		entroq library -> grpc backend
//
// You can start, for example, a postgres-backed QSvc like this (or just use pg/svc):
//
// 	ctx := context.Background()
// 	svc, err := qsvc.New(ctx, pg.Opener(dbHostPort)) // Other options available, too.
// 	if err != nil {
// 		log.Fatalf("Can't open PG backend: %v",e rr)
// 	}
// 	defer svc.Close()
//
// 	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", thisPort))
// 	if err != nil {
// 		log.Fatalf("Can't start this service")
// 	}
//
// 	s := grpc.NewServer()
// 	pb.RegisterEntroQServer(s, svc)
// 	s.Serve(lis)
//
// With the server set up this way, the client simply uses the EntroQ library,
// hands it the grpc Opener, and they're off:
//
// 	client, err := entroq.New(ctx, grpc.Opener("myhost:54321", grpc.WithInsecure()))
//
// That creates a client library that uses a gRPC connection to do its work.
// Note that Claim will block on the *client* side doing this instead of
// holding the *server* connection hostage while a claim fails to go through.
// That is actually what we want; rather than hold connections open, we allow
// the client to poll with exponential backoff. In large-scale systems, this is
// better behavior.
package grpc

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shiblon/entroq"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/shiblon/entroq/proto"
)

const DefaultAddr = ":37706"

// Opener creates an opener function to be used to get a gRPC backend. If the
// address string is empty, it defaults to the DefaultAddr, the default value
// for the memory-backed gRPC server.
func Opener(addr string, dialOpts ...grpc.DialOption) entroq.BackendOpener {
	if addr == "" {
		addr = DefaultAddr
	}
	return func(ctx context.Context) (entroq.Backend, error) {
		conn, err := grpc.DialContext(ctx, addr, dialOpts...)
		if err != nil {
			return nil, fmt.Errorf("failed to dial %q: %v", addr, err)
		}
		return New(conn)
	}
}

type backend struct {
	conn *grpc.ClientConn
	cli  pb.EntroQClient
}

// New creates a new gRPC backend that attaches to the task service via gRPC.
func New(conn *grpc.ClientConn) (*backend, error) {
	return &backend{
		conn: conn,
		cli:  pb.NewEntroQClient(conn),
	}, nil
}

// Close closes the underlying connection to the gRPC task service.
func (b *backend) Close() error {
	return b.conn.Close()
}

// Queues produces a mapping from queue names to queue sizes.
func (b *backend) Queues(ctx context.Context, qq *entroq.QueuesQuery) (map[string]int, error) {
	resp, err := b.cli.Queues(ctx, &pb.QueuesRequest{
		MatchPrefix: qq.MatchPrefix,
		MatchExact:  qq.MatchExact,
		Limit:       int32(qq.Limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get queues over gRPC: %v", err)
	}
	qs := make(map[string]int)
	for _, q := range resp.Queues {
		qs[q.Name] = int(q.NumTasks)
	}
	return qs, nil
}

func fromMS(ms int64) time.Time {
	return time.Unix(0, ms*int64(time.Millisecond))
}

func toMS(t time.Time) int64 {
	return t.Truncate(time.Millisecond).UnixNano() / 1000000
}

func fromTaskProto(t *pb.Task) (*entroq.Task, error) {
	cid, err := uuid.Parse(t.ClaimantId)
	if err != nil {
		return nil, fmt.Errorf("claimant UUID parse failure: %v", err)
	}

	id, err := uuid.Parse(t.Id)
	if err != nil {
		return nil, fmt.Errorf("task UUID parse failure: %v", err)
	}

	return &entroq.Task{
		Queue:    t.Queue,
		ID:       id,
		Version:  t.Version,
		At:       fromMS(t.AtMs),
		Claimant: cid,
		Value:    t.Value,
		Created:  fromMS(t.CreatedMs),
		Modified: fromMS(t.ModifiedMs),
	}, nil
}

func protoFromTaskData(td *entroq.TaskData) *pb.TaskData {
	return &pb.TaskData{
		Queue: td.Queue,
		AtMs:  toMS(td.At),
		Value: td.Value,
	}
}

func changeProtoFromTask(t *entroq.Task) *pb.TaskChange {
	return &pb.TaskChange{
		OldId:   protoFromTaskID(t.IDVersion()),
		NewData: protoFromTaskData(t.Data()),
	}

}

func fromTaskIDProto(tid *pb.TaskID) (*entroq.TaskID, error) {
	id, err := uuid.Parse(tid.Id)
	if err != nil {
		return nil, fmt.Errorf("failed to parse task ID: %v", err)
	}
	return &entroq.TaskID{
		ID:      id,
		Version: tid.Version,
	}, nil
}

func protoFromTaskID(tid *entroq.TaskID) *pb.TaskID {
	return &pb.TaskID{
		Id:      tid.ID.String(),
		Version: tid.Version,
	}
}

// Tasks produces a list of tasks in a given queue, possibly limited by claimant.
func (b *backend) Tasks(ctx context.Context, tq *entroq.TasksQuery) ([]*entroq.Task, error) {
	resp, err := b.cli.Tasks(ctx, &pb.TasksRequest{
		ClaimantId: tq.Claimant.String(),
		Queue:      tq.Queue,
		Limit:      int32(tq.Limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get tasks over gRPC: %v", err)
	}
	var tasks []*entroq.Task
	for _, t := range resp.Tasks {
		task, err := fromTaskProto(t)
		if err != nil {
			return nil, fmt.Errorf("failed to parse task response: %v", err)
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// TryClaim attempts to claim a task from the queue. Returns a nil task if there is
// nothing in the queue (not an error).
func (b *backend) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	resp, err := b.cli.TryClaim(ctx, &pb.ClaimRequest{
		ClaimantId: cq.Claimant.String(),
		Queue:      cq.Queue,
		DurationMs: int64(cq.Duration / time.Millisecond),
	})
	if err != nil {
		return nil, fmt.Errorf("claim failed over gRPC: %v", err)
	}
	if resp.Task == nil {
		return nil, nil
	}
	return fromTaskProto(resp.Task)
}

// Modify modifies the task system with the given batch of modifications.
func (b *backend) Modify(ctx context.Context, mod *entroq.Modification) (inserted []*entroq.Task, changed []*entroq.Task, err error) {
	req := &pb.ModifyRequest{
		ClaimantId: mod.Claimant.String(),
	}
	for _, ins := range mod.Inserts {
		req.Inserts = append(req.Inserts, protoFromTaskData(ins))
	}
	for _, task := range mod.Changes {
		req.Changes = append(req.Changes, changeProtoFromTask(task))
	}
	for _, del := range mod.Deletes {
		req.Deletes = append(req.Deletes, &pb.TaskID{
			Id:      del.ID.String(),
			Version: del.Version,
		})
	}
	for _, dep := range mod.Depends {
		req.Depends = append(req.Depends, &pb.TaskID{
			Id:      dep.ID.String(),
			Version: dep.Version,
		})
	}

	resp, err := b.cli.Modify(ctx, req)
	if err != nil {
		switch stat := status.Convert(err); stat.Code() {
		case codes.NotFound:
			// Dependency error, should have details.
			depErr := new(entroq.DependencyError)
			for _, det := range stat.Details() {
				detail, ok := det.(*pb.ModifyDep)
				if !ok {
					return nil, nil, fmt.Errorf("modification dependency failure, and error reading details: %v", err)
				}
				tid, err := fromTaskIDProto(detail.Id)
				if err != nil {
					return nil, nil, fmt.Errorf("modification dependency failure, and bad ID in details: %v", err)
				}
				switch detail.Type {
				case pb.DepType_CLAIM:
					depErr.Claims = append(depErr.Claims, tid)
				case pb.DepType_DELETE:
					depErr.Deletes = append(depErr.Deletes, tid)
				case pb.DepType_CHANGE:
					depErr.Changes = append(depErr.Changes, tid)
				case pb.DepType_DEPEND:
					depErr.Depends = append(depErr.Depends, tid)
				default:
					return nil, nil, fmt.Errorf("modification dependency failure, encountered unknown detail type: %v --- detail %v", err, detail)
				}
				return nil, nil, depErr
			}
		default:
			return nil, nil, fmt.Errorf("modify via grpc failed: %v", err)
		}
	}

	for _, t := range resp.Inserted {
		task, err := fromTaskProto(t)
		if err != nil {
			return nil, nil, fmt.Errorf("modification success, but failed to parse inserted values: %v", err)
		}
		inserted = append(inserted, task)
	}
	for _, t := range resp.Changed {
		task, err := fromTaskProto(t)
		if err != nil {
			return nil, nil, fmt.Errorf("modification success, but failed to parse changed values: %v", err)
		}
		changed = append(changed, task)
	}

	return inserted, changed, nil
}
