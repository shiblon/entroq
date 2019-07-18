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
package grpc // import "entrogo.com/entroq/grpc"

import (
	"context"
	"fmt"
	"net"
	"time"

	"entrogo.com/entroq"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "entrogo.com/entroq/proto"
	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	// DefaultAddr is the default listening address for gRPC services.
	DefaultAddr = ":37706"

	// ClaimRetryInterval is how long a grpc client holds a claim request open
	// before dropping it and trying again.
	ClaimRetryInterval = 2 * time.Minute
)

type backendOptions struct {
	dialOpts []grpc.DialOption
}

// Option allows grpc-opener-specific options to be sent in Opener.
type Option func(*backendOptions)

// WithDialOpts sets grpc dial options. Can be called multiple times.
// Only valid in call to Opener.
func WithDialOpts(d ...grpc.DialOption) Option {
	return func(opts *backendOptions) {
		opts.dialOpts = append(opts.dialOpts, d...)
	}
}

// WithInsecure is a common gRPC dial option, here for convenience.
func WithInsecure() Option {
	return WithDialOpts(grpc.WithInsecure())
}

// WithDialer is a common gRPC dial option, here for convenience.
func WithDialer(f func(string, time.Duration) (net.Conn, error)) Option {
	return WithDialOpts(grpc.WithDialer(f))
}

// WithBlock is a common gRPC dial option, here for convenience.
func WithBlock() Option {
	return WithDialOpts(grpc.WithBlock())
}

// WithNiladicDialer uses a niladic dial function such as that returned by
// bufconn.Listen. Useful for testing.
func WithNiladicDialer(f func() (net.Conn, error)) Option {
	return WithDialOpts(grpc.WithDialer(func(string, time.Duration) (net.Conn, error) {
		return f()
	}))
}

// Opener creates an opener function to be used to get a gRPC backend. If the
// address string is empty, it defaults to the DefaultAddr, the default value
// for the memory-backed gRPC server.
func Opener(addr string, opts ...Option) entroq.BackendOpener {
	if addr == "" {
		addr = DefaultAddr
	}
	options := new(backendOptions)
	for _, opt := range opts {
		opt(options)
	}

	return func(ctx context.Context) (entroq.Backend, error) {
		conn, err := grpc.DialContext(ctx, addr, options.dialOpts...)
		if err != nil {
			return nil, errors.Wrapf(err, "dial %q", addr)
		}
		hclient := hpb.NewHealthClient(conn)
		resp, err := hclient.Check(ctx, &hpb.HealthCheckRequest{})
		if err != nil {
			return nil, errors.Wrap(err, "health check")
		}
		if st := resp.GetStatus(); st != hpb.HealthCheckResponse_SERVING {
			return nil, errors.Errorf("health serving status: %q", st)
		}
		return New(conn, opts...)
	}
}

type backend struct {
	conn *grpc.ClientConn
}

// New creates a new gRPC backend that attaches to the task service via gRPC.
func New(conn *grpc.ClientConn, opts ...Option) (*backend, error) {
	options := new(backendOptions)
	for _, opt := range opts {
		opt(options)
	}
	return &backend{conn}, nil
}

// Close closes the underlying connection to the gRPC task service.
func (b *backend) Close() error {
	return errors.Wrap(b.conn.Close(), "pg backend close")
}

// Queues produces a mapping from queue names to queue sizes.
func (b *backend) Queues(ctx context.Context, qq *entroq.QueuesQuery) (map[string]int, error) {
	resp, err := pb.NewEntroQClient(b.conn).Queues(ctx, &pb.QueuesRequest{
		MatchPrefix: qq.MatchPrefix,
		MatchExact:  qq.MatchExact,
		Limit:       int32(qq.Limit),
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to get queues over gRPC")
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
		return nil, errors.Wrap(err, "claimant UUID parse failure")
	}

	id, err := uuid.Parse(t.Id)
	if err != nil {
		return nil, errors.Wrap(err, "task UUID parse failure")
	}

	return &entroq.Task{
		Queue:    t.Queue,
		ID:       id,
		Version:  t.Version,
		At:       fromMS(t.AtMs),
		Claimant: cid,
		Claims:   t.Claims,
		Value:    t.Value,
		Created:  fromMS(t.CreatedMs),
		Modified: fromMS(t.ModifiedMs),
	}, nil
}

func protoFromTaskData(td *entroq.TaskData) *pb.TaskData {
	id := ""
	if td.ID != uuid.Nil {
		id = td.ID.String()
	}
	return &pb.TaskData{
		Queue: td.Queue,
		AtMs:  toMS(td.At),
		Value: td.Value,
		Id:    id,
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
		return nil, errors.Wrapf(err, "parse %q", tid.Id)
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
	var ids []string
	for _, tid := range tq.IDs {
		ids = append(ids, tid.String())
	}
	resp, err := pb.NewEntroQClient(b.conn).Tasks(ctx, &pb.TasksRequest{
		ClaimantId: tq.Claimant.String(),
		Queue:      tq.Queue,
		Limit:      int32(tq.Limit),
		TaskId:     ids,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to get tasks over gRPC")
	}
	var tasks []*entroq.Task
	for _, t := range resp.Tasks {
		task, err := fromTaskProto(t)
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse task response")
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// Claim attempts to claim a task and blocks until one is ready or the
// operation is canceled.
func (b *backend) Claim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	for {
		// Check whether the parent context was canceled.
		select {
		case <-ctx.Done():
			return nil, errors.Wrap(ctx.Err(), "grpc claim")
		default:
		}
		ctx, _ := context.WithTimeout(ctx, ClaimRetryInterval)
		resp, err := pb.NewEntroQClient(b.conn).Claim(ctx, &pb.ClaimRequest{
			ClaimantId: cq.Claimant.String(),
			Queue:      cq.Queue,
			DurationMs: int64(cq.Duration / time.Millisecond),
			PollMs:     int64(cq.PollTime / time.Millisecond),
		})
		if err != nil {
			if entroq.IsTimeout(err) {
				// If we just timed out on our little request context, then
				// we can go around again.
				// It's possible that the *parent* context timed out, which
				// is why we check that at the beginning of the loop, as well.
				continue
			}
			return nil, errors.Wrap(err, "grpc claim")
		}
		if resp.Task == nil {
			return nil, errors.New("no task returned from backend Claim")
		}
		return fromTaskProto(resp.Task)
	}
}

// TryClaim attempts to claim a task from the queue. Normally returns both a
// nil task and error if nothing is ready.
func (b *backend) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	resp, err := pb.NewEntroQClient(b.conn).TryClaim(ctx, &pb.ClaimRequest{
		ClaimantId: cq.Claimant.String(),
		Queue:      cq.Queue,
		DurationMs: int64(cq.Duration / time.Millisecond),
	})
	if err != nil {
		return nil, errors.Wrap(err, "grpc try claim")
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

	resp, err := pb.NewEntroQClient(b.conn).Modify(ctx, req)
	if err != nil {
		switch stat := status.Convert(errors.Cause(err)); stat.Code() {
		case codes.NotFound:
			// Dependency error, should have details.
			depErr := entroq.DependencyError{
				Message: err.Error(),
			}
			for _, det := range stat.Details() {
				detail, ok := det.(*pb.ModifyDep)
				if !ok {
					return nil, nil, errors.Errorf("grpc modify unexpected detail type %T: %+v", det, det)
				}
				if detail.Type == pb.DepType_DETAIL {
					if detail.Msg != "" {
						depErr.Message += fmt.Sprintf(": %s", detail.Msg)
					}
					continue
				}

				tid, err := fromTaskIDProto(detail.Id)
				if err != nil {
					return nil, nil, errors.Wrap(err, "grpc modify from proto")
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
				case pb.DepType_INSERT:
					depErr.Inserts = append(depErr.Inserts, tid)
				default:
					return nil, nil, errors.Errorf("grpc modify unknown type %v in detail %v", detail.Type, detail)
				}
			}
			return nil, nil, errors.Wrap(depErr, "grpc modify dependency")
		default:
			return nil, nil, errors.Wrap(err, "grpc modify")
		}
	}

	for _, t := range resp.GetInserted() {
		task, err := fromTaskProto(t)
		if err != nil {
			return nil, nil, errors.Wrap(err, "grpc modify task proto")
		}
		inserted = append(inserted, task)
	}
	for _, t := range resp.GetChanged() {
		task, err := fromTaskProto(t)
		if err != nil {
			return nil, nil, errors.Wrap(err, "grpc modify changed")
		}
		changed = append(changed, task)
	}

	return inserted, changed, nil
}
