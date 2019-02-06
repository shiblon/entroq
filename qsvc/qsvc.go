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
	"log"
	"sync"
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
	sync.Mutex

	// Pull from here to get clients.
	freePool <-chan *entroq.EntroQ
	// Push back here to return clients.
	donePool chan<- *entroq.EntroQ

	// Signal that we're finished with everything.
	done chan struct{}
}

func lock(mu sync.Locker) sync.Locker {
	mu.Lock()
	return mu
}

func un(mu sync.Locker) {
	mu.Unlock()
}

type svcOptions struct {
	connections int
}

// QSvcOpt sets an option for the queue service.
type QSvcOpt func(opts *svcOptions)

// WithConnections sets the maximum connections (sets to 1 if < 1 or not present).
func WithConnections(n int) QSvcOpt {
	return func(opts *svcOptions) {
		if n < 1 {
			n = 1
		}
		opts.connections = n
	}
}

// New creates a new service that exposes gRPC endpoints for task queue access.
// It load balances across connections (presumably attached to a specific
// persistent backend) using round robin in order of descending "time not
// busy". The specified opener is used to select the backend type and pass
// parameters to it. If connections is less than 1, it is set to 1. This is an
// important default because some implementations might *only* ever allow 1
// connection, and you need to know what you're doing anytime you set this
// higher (you need to know, for example, whether the backend can handle
// multiple clients---something like an in-memory backend certainly can't).
func New(ctx context.Context, opener entroq.BackendOpener, opts ...QSvcOpt) (svc *QSvc, err error) {
	options := &svcOptions{
		connections: 1,
	}
	for _, o := range opts {
		o(options)
	}

	// Create read/write channels, then set to read-only, write-only values below.
	// The goroutine in here that manages these can do both operations on them,
	// but other methods can't, which protects against mistakes.
	freePool := make(chan *entroq.EntroQ, options.connections)
	donePool := make(chan *entroq.EntroQ, options.connections)

	svc = &QSvc{
		freePool: freePool,
		donePool: donePool,
		done:     make(chan struct{}),
	}

	for i := 0; i < options.connections; i++ {
		cli, err := entroq.New(ctx, opener)
		if err != nil {
			// Oops - close everything we've opened.
			for j := 0; j < i; j++ {
				c := <-freePool
				c.Close()
			}
			return nil, fmt.Errorf("qsvc backend client: %v", err)
		}
		freePool <- cli
	}

	go func() {
		for {
			select {
			case <-svc.done:
				// Subtle. To avoid having to lock things, we close the free
				// pool so that no more can be pushed onto it, and then we
				// consume everything that we can. It's possible that we'll get
				// scooped by a request, though. That's fine. Stuff will
				// happen, then the client will be sent back on the "done
				// pool". We consume all the rest of them and close them, too.
				// This allows all client connection users to complete gracefully.
				//
				// Note that this means we can't select on context and then not
				// do anything in requests. We have to return clients to the
				// pool even when the context is canceled.
				close(freePool)
				numFree := 0
				for c := range freePool {
					numFree++
					c.Close()
				}
				freePool = nil
				for i := 0; i < options.connections-numFree; i++ {
					c := <-donePool
					c.Close()
				}
				donePool = nil
				return
			case c := <-donePool:
				// A connection is being returned. Put it back.
				freePool <- c
			}
		}
	}()

	return svc, nil
}

// Close closes the backend connections and flushes the connection free.
func (s *QSvc) Close() error {
	close(s.done)
	return nil
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

func (s *QSvc) getClient(ctx context.Context) (*entroq.EntroQ, error) {
	select {
	case c, ok := <-s.freePool:
		if !ok {
			return nil, fmt.Errrorf("no backend connections")
		}
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *QSvc) returnClient(c *entroq.EntroQ) {
	// This should always work because it's buffered.
	select {
	case s.donePool <- c:
	default:
		log.Fatal("Bug found: failed to return client to buffered done pool")
	}
}

// TryClaim attempts to claim a task, returning immediately. If no tasks are
// available, it returns a nil response and a nil error.
func (s *QSvc) TryClaim(ctx context.Context, req *pb.ClaimRequest) (*pb.ClaimResponse, error) {
	client, err := s.getClient(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "no clients for TryClaim: %v", err)
	}
	defer s.returnClient(client)

	claimant, err := uuid.Parse(req.ClaimantId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to parse claimant ID: %v", err)
	}
	task, err := client.TryClaim(ctx, req.Queue, time.Duration(req.DurationMs)*time.Millisecond, entroq.ClaimAs(claimant))
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
	client, err := s.getClient(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "no clients for Modify: %v", err)
	}
	defer s.returnClient(client)

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
	inserted, changed, err := client.Modify(ctx, modArgs...)
	if err != nil {
		if depErr, ok := err.(*entroq.DependencyError); ok {
			tmap := map[pb.DepType][]*entroq.TaskID{
				pb.DepType_DEPEND: depErr.Depends,
				pb.DepType_DELETE: depErr.Deletes,
				pb.DepType_CHANGE: depErr.Changes,
				pb.DepType_CLAIM:  depErr.Claims,
			}

			var details []proto.Message
			for dtype, dvals := range tmap {
				for _, tid := range dvals {
					details = append(details, &pb.ModifyDep{
						Type: dtype,
						Id:   &pb.TaskID{Id: tid.String(), Version: tid.Version},
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
	client, err := s.getClient(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "no clients for Tasks: %v", err)
	}
	defer s.returnClient(client)

	claimant := uuid.Nil
	if req.ClaimantId != "" {
		var err error
		if claimant, err = uuid.Parse(req.ClaimantId); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "failed to parse claimant ID: %v", err)
		}
	}
	// Claimant will only really be limited if it is nonzero.
	tasks, err := client.Tasks(ctx, req.Queue, entroq.LimitClaimant(claimant))
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
	client, err := s.getClient(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "no clients for Queues: %v", err)
	}
	defer s.returnClient(client)

	queueMap, err := client.Queues(ctx,
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
