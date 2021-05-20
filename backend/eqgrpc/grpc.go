// Package eqgrpc provides a gRPC backend for EntroQ. This is the backend that is
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
// 	s := eqgrpc.NewServer()
// 	pb.RegisterEntroQServer(s, svc)
// 	s.Serve(lis)
//
// With the server set up this way, the client simply uses the EntroQ library,
// hands it the eqgrpc Opener, and they're off:
//
// 	client, err := entroq.New(ctx, eqgrpc.Opener("myhost:54321", eqgrpc.WithInsecure()))
//
// That creates a client library that uses a gRPC connection to do its work.
// Note that Claim will block on the *client* side doing this instead of
// holding the *server* connection hostage while a claim fails to go through.
// That is actually what we want; rather than hold connections open, we allow
// the client to poll with exponential backoff. In large-scale systems, this is
// better behavior.
package eqgrpc // import "entrogo.com/entroq/backend/eqgrpc"

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"entrogo.com/entroq"
	"entrogo.com/entroq/pkg/authz"
	"github.com/google/uuid"
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

	// MB helps with conversion to and from megabytes.
	MB = 1024 * 1024
)

type backendOptions struct {
	dialOpts    []grpc.DialOption
	bearerToken string
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

// WithMaxSize is a convenience method for setting
// WithDialOptions(grpc.WithDefaultCallOptions(grpc.MaxCallRecvSize(...), grpc.MaxCallSendSize(...))).
// Default is 4MB.
func WithMaxSize(maxMB int) Option {
	return WithDialOpts(grpc.WithDefaultCallOptions(
		grpc.MaxCallRecvMsgSize(maxMB*MB),
		grpc.MaxCallSendMsgSize(maxMB*MB),
	))
}

// WithBearerToken sets a bearer token to use for all requests.
func WithBearerToken(tok string) Option {
	return func(opts *backendOptions) {
		opts.bearerToken = tok
	}
}

// BearerCredentials implements the RPC Credentials interface, and provides a bearer token for gRPC communication.
type BearerCredentials struct {
	token string
}

// NewBearerCredentials creates credentials for a bearer token.
func NewBearerCredentials(tok string) *BearerCredentials {
	return &BearerCredentials{token: tok}
}

// GetRequestMetadata provides an authorization header for a bearer token.
func (c *BearerCredentials) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + c.token}, nil
}

// RequireTransportSecurity is always false, tread carefully! If not on localhost, ensure security is on.
func (*BearerCredentials) RequireTransportSecurity() bool { return false }

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

	switch {
	case options.bearerToken != "":
		options.dialOpts = append(options.dialOpts, grpc.WithPerRPCCredentials(
			NewBearerCredentials(options.bearerToken),
		))
	}

	return func(ctx context.Context) (entroq.Backend, error) {
		conn, err := grpc.DialContext(ctx, addr, options.dialOpts...)
		if err != nil {
			return nil, fmt.Errorf("dial %q: %w", addr, err)
		}
		hclient := hpb.NewHealthClient(conn)
		resp, err := hclient.Check(ctx, &hpb.HealthCheckRequest{})
		if err != nil {
			return nil, fmt.Errorf("health check: %w", err)
		}
		if st := resp.GetStatus(); st != hpb.HealthCheckResponse_SERVING {
			return nil, fmt.Errorf("health serving status: %q", st)
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
	if err := b.conn.Close(); err != nil {
		return fmt.Errorf("grpc backend close: %w", err)
	}
	return nil
}

// Queues produces a mapping from queue names to queue sizes.
func (b *backend) Queues(ctx context.Context, qq *entroq.QueuesQuery) (map[string]int, error) {
	resp, err := pb.NewEntroQClient(b.conn).Queues(ctx, &pb.QueuesRequest{
		MatchPrefix: qq.MatchPrefix,
		MatchExact:  qq.MatchExact,
		Limit:       int32(qq.Limit),
	})
	if err != nil {
		return nil, fmt.Errorf("grpc queues: %w", unpackGRPCError(err))
	}
	qs := make(map[string]int)
	for _, q := range resp.Queues {
		qs[q.Name] = int(q.NumTasks)
	}
	return qs, nil
}

// QueueStats maps queue names to stats for those queues.
func (b *backend) QueueStats(ctx context.Context, qq *entroq.QueuesQuery) (map[string]*entroq.QueueStat, error) {
	resp, err := pb.NewEntroQClient(b.conn).QueueStats(ctx, &pb.QueuesRequest{
		MatchPrefix: qq.MatchPrefix,
		MatchExact:  qq.MatchExact,
		Limit:       int32(qq.Limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get queue stats over gRPC: %w", err)
	}
	qs := make(map[string]*entroq.QueueStat)
	for _, q := range resp.Queues {
		qs[q.Name] = &entroq.QueueStat{
			Name:      q.Name,
			Size:      int(q.NumTasks),
			Claimed:   int(q.NumClaimed),
			Available: int(q.NumAvailable),
			MaxClaims: int(q.MaxClaims),
		}
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
		return nil, fmt.Errorf("claimant UUID parse failure: %w", err)
	}

	id, err := uuid.Parse(t.Id)
	if err != nil {
		return nil, fmt.Errorf("task UUID parse failure: %w", err)
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
		// Omit FromQueue - not needed here.
		Attempt: t.Attempt,
		Err:     t.Err,
	}, nil
}

func protoFromTaskData(td *entroq.TaskData) *pb.TaskData {
	id := ""
	if td.ID != uuid.Nil {
		id = td.ID.String()
	}
	return &pb.TaskData{
		Queue:   td.Queue,
		AtMs:    toMS(td.At),
		Value:   td.Value,
		Attempt: td.Attempt,
		Err:     td.Err,
		Id:      id,
	}
}

func changeProtoFromTask(t *entroq.Task) *pb.TaskChange {
	return &pb.TaskChange{
		OldId: &pb.TaskID{
			Id:      t.ID.String(),
			Version: t.Version,
			Queue:   t.FromQueue, // old queue goes in the ID for changes.
		},
		NewData: protoFromTaskData(t.Data()),
	}
}

func fromTaskIDProto(tid *pb.TaskID) (*entroq.TaskID, error) {
	id, err := uuid.Parse(tid.Id)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", tid.Id, err)
	}
	return &entroq.TaskID{
		ID:      id,
		Version: tid.Version,
		Queue:   tid.Queue,
	}, nil
}

// Tasks produces a list of tasks in a given queue, possibly limited by claimant.
func (b *backend) Tasks(ctx context.Context, tq *entroq.TasksQuery) ([]*entroq.Task, error) {
	var ids []string
	for _, tid := range tq.IDs {
		ids = append(ids, tid.String())
	}

	stream, err := pb.NewEntroQClient(b.conn).StreamTasks(ctx, &pb.TasksRequest{
		ClaimantId: tq.Claimant.String(),
		Queue:      tq.Queue,
		Limit:      int32(tq.Limit),
		TaskId:     ids,
		OmitValues: tq.OmitValues,
	})
	if err != nil {
		return nil, fmt.Errorf("stream tasks: %w", err)
	}
	var tasks []*entroq.Task
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("receive tasks: %w", unpackGRPCError(err))
		}
		for _, t := range resp.Tasks {
			task, err := fromTaskProto(t)
			if err != nil {
				return nil, fmt.Errorf("parse tasks: %w", err)
			}
			tasks = append(tasks, task)
		}
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
			return nil, fmt.Errorf("grpc claim: %w", ctx.Err())
		default:
		}
		ctx, _ := context.WithTimeout(ctx, ClaimRetryInterval)
		resp, err := pb.NewEntroQClient(b.conn).Claim(ctx, &pb.ClaimRequest{
			ClaimantId: cq.Claimant.String(),
			Queues:     cq.Queues,
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
			return nil, fmt.Errorf("grpc claim: %w", unpackGRPCError(err))
		}
		if resp.Task == nil {
			return nil, fmt.Errorf("no task returned from backend Claim")
		}
		return fromTaskProto(resp.Task)
	}
}

// TryClaim attempts to claim a task from the queue. Normally returns both a
// nil task and error if nothing is ready.
func (b *backend) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	resp, err := pb.NewEntroQClient(b.conn).TryClaim(ctx, &pb.ClaimRequest{
		ClaimantId: cq.Claimant.String(),
		Queues:     cq.Queues,
		DurationMs: int64(cq.Duration / time.Millisecond),
	})
	if err != nil {
		return nil, fmt.Errorf("grpc try claim: %w", unpackGRPCError(err))
	}
	if resp.Task == nil {
		return nil, nil
	}
	return fromTaskProto(resp.Task)
}

func authzErrFromStat(stat *status.Status) error {
	if stat.Code() != codes.PermissionDenied {
		return fmt.Errorf("expected PermissionDenied, got something else: %w", stat.Err())
	}
	authzErr := new(authz.AuthzError)
	for _, det := range stat.Details() {
		detail, ok := det.(*pb.AuthzDep)
		if !ok {
			return fmt.Errorf("grpc unexpected authz type %T: %+v", det, det)
		}
		if len(detail.Actions) == 1 && detail.Actions[0] == pb.ActionType_DETAIL && detail.Msg != "" {
			authzErr.Errors = append(authzErr.Errors, detail.Msg)
			continue
		}
		q := &authz.Queue{
			Exact:  detail.Exact,
			Prefix: detail.Prefix,
		}
		for _, a := range detail.Actions {
			q.Actions = append(q.Actions, authz.Action(a.String()))
		}
		authzErr.Failed = append(authzErr.Failed, q)
	}
	return authzErr
}

func depErrorFromStat(stat *status.Status) error {
	if stat.Code() != codes.NotFound {
		return fmt.Errorf("expected NotFound, got something else: %w", stat.Err())
	}
	// Dependency error, should have details.
	depErr := entroq.DependencyError{
		Message: stat.Err().Error(),
	}
	for _, det := range stat.Details() {
		detail, ok := det.(*pb.ModifyDep)
		if !ok {
			return fmt.Errorf("grpc unexpected dependency type %T: %+v", det, det)
		}
		if detail.Type == pb.ActionType_DETAIL {
			if detail.Msg != "" {
				depErr.Message += fmt.Sprintf(": %s", detail.Msg)
			}
			continue
		}

		tid, err := fromTaskIDProto(detail.Id)
		if err != nil {
			return fmt.Errorf("grpc dependency from proto: %w", err)
		}
		switch detail.Type {
		case pb.ActionType_CLAIM:
			depErr.Claims = append(depErr.Claims, tid)
		case pb.ActionType_DELETE:
			depErr.Deletes = append(depErr.Deletes, tid)
		case pb.ActionType_CHANGE:
			depErr.Changes = append(depErr.Changes, tid)
		case pb.ActionType_DEPEND:
			depErr.Depends = append(depErr.Depends, tid)
		case pb.ActionType_INSERT:
			depErr.Inserts = append(depErr.Inserts, tid)
		default:
			return fmt.Errorf("grpc dependency unknown type %v in detail %v", detail.Type, detail)
		}
	}
	return depErr
}

func unpackGRPCError(grpcErr error) error {
	if grpcErr == nil {
		return nil
	}
	stat, ok := status.FromError(grpcErr)
	if !ok {
		return grpcErr
	}
	switch stat.Code() {
	case codes.NotFound:
		return depErrorFromStat(stat)
	case codes.PermissionDenied:
		return authzErrFromStat(stat)
	default:
		return grpcErr
	}
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
			Queue:   del.Queue,
		})
	}
	for _, dep := range mod.Depends {
		req.Depends = append(req.Depends, &pb.TaskID{
			Id:      dep.ID.String(),
			Version: dep.Version,
			Queue:   dep.Queue,
		})
	}

	resp, err := pb.NewEntroQClient(b.conn).Modify(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("grpc modify: %w", unpackGRPCError(err))
	}

	for _, t := range resp.GetInserted() {
		task, err := fromTaskProto(t)
		if err != nil {
			return nil, nil, fmt.Errorf("grpc modify task proto: %w", err)
		}
		inserted = append(inserted, task)
	}
	for _, t := range resp.GetChanged() {
		task, err := fromTaskProto(t)
		if err != nil {
			return nil, nil, fmt.Errorf("grpc modify changed: %w", err)
		}
		changed = append(changed, task)
	}

	return inserted, changed, nil
}

// Time returns the time as reported by the server.
func (b *backend) Time(ctx context.Context) (time.Time, error) {
	resp, err := pb.NewEntroQClient(b.conn).Time(ctx, new(pb.TimeRequest))
	if err != nil {
		return time.Time{}, fmt.Errorf("grpc time: %w", unpackGRPCError(err))
	}
	return fromMS(resp.TimeMs).UTC(), nil
}
