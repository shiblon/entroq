// Package eqgrpc provides a gRPC backend for EntroQ. This is the backend that is
// commonly used by clients of an EntroQ task service, set up thus:
//
//	Server:
//		eqsvcgrpc -> entroq library -> some backend (e.g., pg)
//
//	Client:
//		entroq library -> grpc backend
//
// You can start, for example, a postgres-backed QSvc like this (or just use pg/svc):
//
//	ctx := context.Background()
//	svc, err := eqsvcgrpc.New(ctx, eqpg.Opener(dbHostPort)) // Other options available, too.
//	if err != nil {
//		log.Fatalf("Can't open PG backend: %v", err)
//	}
//	defer svc.Close()
//
//	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", thisPort))
//	if err != nil {
//		log.Fatalf("Can't start this service")
//	}
//
//	s := grpc.NewServer(eqgrpc.ServerKeepalive())
//	pb.RegisterEntroQServer(s, svc)
//	s.Serve(lis)
//
// With the server set up this way, the client simply uses the EntroQ library,
// hands it the eqgrpc Opener, and they're off:
//
//	client, err := entroq.New(ctx, eqgrpc.Opener("myhost:54321", eqgrpc.WithInsecure()))
//
// That creates a client library that uses a gRPC connection to do its work.
// Claim is one long-held RPC, canceled only by the caller. Servers must accept
// the client keepalive interval while that RPC is active; see
// DefaultKeepaliveTime.
package eqgrpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/authz"
	"github.com/shiblon/entroq/pkg/pbconv"
	"github.com/shiblon/entroq/pkg/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/shiblon/entroq/api"
	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	// DefaultAddr is the default listening address for gRPC services.
	DefaultAddr = ":37706"

	// DefaultKeepaliveTime is how long an idle client transport waits before
	// probing a server while an RPC such as Claim remains active. A server must
	// configure keepalive.EnforcementPolicy.MinTime to this value or lower. The
	// grpc-go server default is five minutes and is not compatible.
	DefaultKeepaliveTime = 30 * time.Second

	// DefaultKeepaliveTimeout is how long a keepalive probe waits for activity
	// before treating the transport as dead.
	DefaultKeepaliveTimeout = 20 * time.Second

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

// ServerKeepalive is the server option that accepts this client's keepalive
// pings while an RPC such as Claim is open. The grpc-go server default refuses
// pings more often than every five minutes and closes the connection on the
// third, failing any claim that waits longer than a few pings. Every server
// for EntroQ clients needs it:
//
//	s := grpc.NewServer(eqgrpc.ServerKeepalive())
func ServerKeepalive() grpc.ServerOption {
	return grpc.KeepaliveEnforcementPolicy(serverKeepalivePolicy())
}

// serverKeepalivePolicy accepts pings as often as DefaultKeepaliveTime, but
// only while an RPC is open, as the client only sends them then.
func serverKeepalivePolicy() keepalive.EnforcementPolicy {
	return keepalive.EnforcementPolicy{MinTime: DefaultKeepaliveTime}
}

// WithMaxSize is a convenience method for setting
// WithDialOptions(grpc.WithDefaultCallOptions(grpc.MaxCallRecvSize(...), grpc.MaxCallSendSize(...))).
// By default the client limits neither, leaving the server's limits to govern.
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
func (*BearerCredentials) RequireTransportSecurity() bool {
	return false
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

	if options.bearerToken != "" {
		options.dialOpts = append(options.dialOpts, grpc.WithPerRPCCredentials(
			NewBearerCredentials(options.bearerToken),
		))
	}

	return func(ctx context.Context) (entroq.Backend, error) {
		// Keepalive lets the client notice a dead connection during a long-held
		// blocking Claim RPC and fail it, so the error surfaces to the caller
		// instead of hanging on a silently-broken connection. This matters because
		// Claim has no per-attempt deadline (see backend.Claim): a dead connection
		// is caught here, by keepalive, not by canceling the RPC. PermitWithoutStream
		// stays false, so pings go only while an RPC such as Claim is active. Servers
		// must accept DefaultKeepaliveTime; the official server does. Prepended so a
		// caller's WithDialOpts can override it.
		//
		// Responses are not size-limited by default: the server's send limit
		// (--max_size_mb) already bounds them, and gRPC's 4MB client default
		// would reject responses the server is configured to send. WithMaxSize
		// sets a limit.
		dialOpts := append([]grpc.DialOption{
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time:    DefaultKeepaliveTime,
				Timeout: DefaultKeepaliveTimeout,
			}),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(math.MaxInt32)),
		}, options.dialOpts...)
		conn, err := grpc.DialContext(ctx, addr, dialOpts...)
		if err != nil {
			return nil, fmt.Errorf("dial %q: %w", addr, err)
		}
		hclient := hpb.NewHealthClient(conn)
		resp, err := hclient.Check(ctx, &hpb.HealthCheckRequest{})
		if err != nil {
			return nil, closeFailedConnection(conn, fmt.Errorf("health check: %w", err))
		}
		if st := resp.GetStatus(); st != hpb.HealthCheckResponse_SERVING {
			return nil, closeFailedConnection(conn, fmt.Errorf("health serving status: %q", st))
		}
		backend, err := New(conn, opts...)
		if err != nil {
			return nil, closeFailedConnection(conn, err)
		}
		return backend, nil
	}
}

func closeFailedConnection(conn *grpc.ClientConn, openErr error) error {
	if err := conn.Close(); err != nil {
		return errors.Join(openErr, fmt.Errorf("close failed gRPC connection: %w", err))
	}
	return openErr
}

type backend struct {
	conn *grpc.ClientConn
	// protocol is the wire protocol the server last reported in its response
	// headers: 0 before any response, 1 for a server that reports none.
	protocol atomic.Int32
}

// client returns a gRPC client whose calls record the server's protocol.
func (b *backend) client() pb.EntroQClient {
	return pb.NewEntroQClient(listeningConn{b.conn, b})
}

// listeningConn records the protocol a server reports in the response headers
// of every unary call made through it.
type listeningConn struct {
	*grpc.ClientConn
	b *backend
}

// Invoke makes a unary call and records the protocol from its response
// headers, when there were any.
func (c listeningConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	var md metadata.MD
	err := c.ClientConn.Invoke(ctx, method, args, reply, append(opts, grpc.Header(&md))...)
	if md.Len() > 0 {
		c.b.protocol.Store(protocolOf(md))
	}
	return err
}

// protocolOf reads the protocol from response headers; a server that sends
// none speaks protocol 1.
func protocolOf(md metadata.MD) int32 {
	vals := md.Get(version.ProtocolHeader)
	if len(vals) == 0 {
		return 1
	}
	p, err := strconv.Atoi(vals[0])
	if err != nil || p < 1 {
		return 1
	}
	return int32(p)
}

// ServerProtocol returns the protocol the server speaks (see
// entroq.ProtocolReporter).
func (b *backend) ServerProtocol(ctx context.Context) (int32, error) {
	return b.serverProtocol(ctx)
}

// serverProtocol returns the protocol the server speaks, asking it with a
// Time call if no response has said yet.
func (b *backend) serverProtocol(ctx context.Context) (int32, error) {
	if p := b.protocol.Load(); p > 0 {
		return p, nil
	}
	if _, err := b.client().Time(ctx, new(pb.TimeRequest)); err != nil {
		return 0, fmt.Errorf("grpc server protocol: %w", unpackGRPCError(err))
	}
	return b.protocol.Load(), nil
}

// needProtocol refuses what a server below protocol p cannot safely receive:
// a 1.12 server applies a change carrying only a newer field as one with
// empty data.
func (b *backend) needProtocol(ctx context.Context, p int32, what string) error {
	got, err := b.serverProtocol(ctx)
	if err != nil {
		return err
	}
	if got < p {
		return entroq.Unsupportedf("grpc: %s needs a server at protocol %d, and this one speaks protocol %d", what, p, got)
	}
	return nil
}

// New creates a new gRPC backend that attaches to the task service via gRPC.
// Options are consumed by Opener at dial time (dial options and credentials);
// the backend itself holds no option-derived state, so opts is accepted for API
// symmetry but not read here.
func New(conn *grpc.ClientConn, _ ...Option) (*backend, error) {
	return &backend{conn: conn}, nil
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
	resp, err := b.client().Queues(ctx, &pb.QueuesRequest{
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
	resp, err := b.client().QueueStats(ctx, &pb.QueuesRequest{
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
			Future:    int(q.NumFuture),
			MaxClaims: int(q.MaxClaims),
		}
	}
	return qs, nil
}

// Tasks produces a list of tasks in a given queue, possibly limited by claimant.
func (b *backend) Tasks(ctx context.Context, tq *entroq.TasksQuery) ([]*entroq.Task, error) {
	stream, err := b.client().StreamTasks(ctx, &pb.TasksRequest{
		ClaimantId: tq.Claimant,
		TaskId:     tq.IDs,
		Queue:      tq.Queue,
		Limit:      int32(tq.Limit),
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
			task, err := pbconv.TaskFromProto(t)
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
	// A single blocking Claim, bounded only by the caller's context -- no
	// per-attempt deadline and no retry loop. Canceling an in-flight Claim would
	// race the server's delivery of a claim it has already committed, stranding
	// that task (claimed, unavailable) until its claim duration expires. A
	// silently-dead connection is surfaced by client keepalive (see Opener),
	// which fails this RPC so the error reaches the caller -- a worker then stops
	// and lets orchestration restart it -- rather than hanging forever. The
	// residual two-generals case (a committed claim whose response never arrives)
	// is a bounded latency cost, not a correctness one: the task self-heals when
	// its claim duration expires and can be claimed again.
	resp, err := b.client().Claim(ctx, &pb.ClaimRequest{
		ClaimantId: cq.Claimant,
		Queues:     cq.Queues,
		DurationMs: int64(cq.Duration / time.Millisecond),
		PollMs:     int64(cq.PollTime / time.Millisecond),
	})
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, fmt.Errorf("grpc claim caller: %w", cerr)
		}
		return nil, fmt.Errorf("grpc claim response: %w", unpackGRPCError(err))
	}
	if resp.Task == nil {
		return nil, fmt.Errorf("no task returned from backend Claim")
	}
	return pbconv.TaskFromProto(resp.Task)
}

// TryClaim attempts to claim a task from the queue. Normally returns both a
// nil task and nil error if nothing is ready.
func (b *backend) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	resp, err := b.client().TryClaim(ctx, &pb.ClaimRequest{
		ClaimantId: cq.Claimant,
		Queues:     cq.Queues,
		DurationMs: int64(cq.Duration / time.Millisecond),
	})
	if err != nil {
		return nil, fmt.Errorf("grpc try claim: %w", unpackGRPCError(err))
	}
	if resp.Task == nil {
		return nil, nil
	}
	return pbconv.TaskFromProto(resp.Task)
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
	// Dependency errors currently arrive as NotFound; a future server minor
	// version may switch to Aborted (a better fit for an optimistic-concurrency
	// conflict). Accept both now so that switch needs no client change.
	if c := stat.Code(); c != codes.NotFound && c != codes.Aborted {
		return fmt.Errorf("expected NotFound or Aborted, got something else: %w", stat.Err())
	}
	// Dependency error, should have details.
	depErr := &entroq.DependencyError{
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

		// Doc set dependency: a DocID naming a set by key failed as a whole.
		if key := detail.GetDocId().GetKey(); key != "" {
			ref := entroq.NewDocSetRef(detail.DocId.GetNamespace(), key, detail.DocId.GetVersion())
			switch detail.Type {
			case pb.ActionType_CLAIM:
				depErr.DocClaims = append(depErr.DocClaims, ref)
			case pb.ActionType_CHANGE:
				depErr.DocArrives = append(depErr.DocArrives, ref)
			default:
				return fmt.Errorf("grpc doc set dependency unknown type %v in detail %v", detail.Type, detail)
			}
			continue
		}

		// Doc dependency: doc_id is set, id is nil.
		if detail.DocId != nil {
			did := &entroq.DocID{
				Namespace: detail.DocId.Namespace,
				ID:        detail.DocId.GetId(),
				Version:   detail.DocId.Version,
			}
			switch detail.Type {
			case pb.ActionType_INSERT:
				depErr.DocInserts = append(depErr.DocInserts, did)
			case pb.ActionType_CLAIM:
				depErr.DocClaims = append(depErr.DocClaims, did)
			case pb.ActionType_DELETE:
				depErr.DocDeletes = append(depErr.DocDeletes, did)
			case pb.ActionType_CHANGE:
				depErr.DocChanges = append(depErr.DocChanges, did)
			case pb.ActionType_DEPEND:
				depErr.DocDepends = append(depErr.DocDepends, did)
			default:
				return fmt.Errorf("grpc doc dependency unknown type %v in detail %v", detail.Type, detail)
			}
			continue
		}

		tid := pbconv.TaskIDFromProto(detail.Id)
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
	case codes.Canceled:
		return fmt.Errorf("%w", context.Canceled)
	case codes.DeadlineExceeded:
		return fmt.Errorf("%w", context.DeadlineExceeded)
	case codes.NotFound, codes.Aborted:
		return depErrorFromStat(stat)
	case codes.PermissionDenied:
		return authzErrFromStat(stat)
	case codes.InvalidArgument:
		return entroq.InvalidArgumentf("%s", stat.Message())
	case codes.Unimplemented:
		return entroq.Unsupportedf("%s", stat.Message())
	case codes.Unavailable:
		// The server is unreachable (down, restarting, or being relocated).
		// Translate to entroq's transient-unavailable error so callers can retry
		// on it via entroq.IsUnavailable without inspecting gRPC codes.
		return entroq.Unavailablef("backend unavailable: %s", stat.Message())
	default:
		return grpcErr
	}
}

// Modify modifies the task system with the given batch of modifications.
func (b *backend) Modify(ctx context.Context, mod *entroq.Modification) (*entroq.ModifyResponse, error) {
	req := &pb.ModifyRequest{
		ClaimantId: mod.Claimant,
	}
	if len(mod.Arrives) > 0 || len(mod.DocArrives) > 0 {
		if err := b.needProtocol(ctx, 2, "an arrival change"); err != nil {
			return nil, err
		}
	}
	for _, a := range mod.Arrives {
		req.Changes = append(req.Changes, pbconv.TaskArrivalToProto(a))
	}
	for _, a := range mod.DocArrives {
		req.DocChanges = append(req.DocChanges, pbconv.DocArrivalToProto(a))
	}
	for _, ins := range mod.Inserts {
		pd, err := pbconv.TaskDataToProto(ins)
		if err != nil {
			return nil, fmt.Errorf("grpc modify insert value: %w", err)
		}
		req.Inserts = append(req.Inserts, pd)
	}
	for _, task := range mod.Changes {
		pc, err := pbconv.TaskChangeToProto(task)
		if err != nil {
			return nil, fmt.Errorf("grpc modify change value: %w", err)
		}
		req.Changes = append(req.Changes, pc)
	}
	for _, del := range mod.Deletes {
		req.Deletes = append(req.Deletes, &pb.TaskID{
			Id:      del.ID,
			Version: del.Version,
			Queue:   del.Queue,
		})
	}
	for _, dep := range mod.Depends {
		req.Depends = append(req.Depends, &pb.TaskID{
			Id:      dep.ID,
			Version: dep.Version,
			Queue:   dep.Queue,
		})
	}

	for _, di := range mod.DocInserts {
		val, err := pbconv.JSONToProto(di.Content)
		if err != nil {
			return nil, fmt.Errorf("doc insert value: %w", err)
		}
		req.DocInserts = append(req.DocInserts, &pb.DocData{
			Namespace:    di.Namespace,
			Id:           di.ID,
			AtMs:         pbconv.ToMS(di.At),
			Key:          di.Key,
			SecondaryKey: di.SecondaryKey,
			Content:      val,
			CreatedMs:    pbconv.ToMS(di.Created),
			ModifiedMs:   pbconv.ToMS(di.Modified),
		})
	}
	for _, dc := range mod.DocChanges {
		val, err := pbconv.JSONToProto(dc.Content)
		if err != nil {
			return nil, fmt.Errorf("doc change value: %w", err)
		}
		req.DocChanges = append(req.DocChanges, &pb.DocChange{
			OldId: pbconv.DocIDToProto(dc.Namespace, dc.ID, dc.Version),
			Data: &pb.DocChange_NewData{NewData: &pb.DocData{
				Key:          dc.Key,
				SecondaryKey: dc.SecondaryKey,
				Content:      val,
				AtMs:         pbconv.ToMS(dc.At),
			}},
		})
	}
	for _, dd := range mod.DocDeletes {
		req.DocDeletes = append(req.DocDeletes, pbconv.DocIDToProto(dd.Namespace, dd.ID, dd.Version))
	}
	for _, ddep := range mod.DocDepends {
		req.DocDepends = append(req.DocDepends, pbconv.DocIDToProto(ddep.Namespace, ddep.ID, ddep.Version))
	}

	resp, err := b.client().Modify(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("grpc modify: %w", unpackGRPCError(err))
	}

	mResp := new(entroq.ModifyResponse)
	for _, t := range resp.GetInserted() {
		task, err := pbconv.TaskFromProto(t)
		if err != nil {
			return nil, fmt.Errorf("grpc modify task proto: %w", err)
		}
		mResp.InsertedTasks = append(mResp.InsertedTasks, task)
	}
	for _, t := range resp.GetChanged() {
		task, err := pbconv.TaskFromProto(t)
		if err != nil {
			return nil, fmt.Errorf("grpc modify changed: %w", err)
		}
		mResp.ChangedTasks = append(mResp.ChangedTasks, task)
	}
	for _, d := range resp.GetInsertedDocs() {
		mResp.InsertedDocs = append(mResp.InsertedDocs, pbconv.MustDocFromProto(d))
	}
	for _, d := range resp.GetChangedDocs() {
		// A doc set whose lease changed comes back as a Doc with no ID.
		if pbconv.IsDocSet(d) {
			mResp.ChangedSets = append(mResp.ChangedSets, pbconv.DocSetFromProto(d, nil))
			continue
		}
		mResp.ChangedDocs = append(mResp.ChangedDocs, pbconv.MustDocFromProto(d))
	}

	return mResp, nil
}

// Time returns the time as reported by the server.
func (b *backend) Time(ctx context.Context) (time.Time, error) {
	resp, err := b.client().Time(ctx, new(pb.TimeRequest))
	if err != nil {
		return time.Time{}, fmt.Errorf("grpc time: %w", unpackGRPCError(err))
	}
	return pbconv.FromMS(resp.TimeMs).UTC(), nil
}
