// Package eqsvcgrpc contains the service implementation for registering with gRPC.
// This provides the service that can be registered with a grpc.Server:
//
//	import (
//		"context"
//		"log"
//		"net"
//
//		"github.com/shiblon/entroq/pkg/backend/eqpg"
//		"github.com/shiblon/entroq/pkg/eqsvcgrpc"
//
//		pb "github.com/shiblon/entroq/api"
//
//		"google.golang.org/grpc"
//	)
//
//	func main() {
//		ctx := context.Background()
//
//		listener, err := net.Listen("tcp", "localhost:54321")
//		if err != nil {
//			log.Fatalf("Failed to listen: %v", err)
//		}
//
//		svc, err := eqsvcgrpc.New(ctx, eqpg.Opener("localhost:5432", eqpg.WithUsername("postgres"), eqpg.WithPassword("postgres")))
//		if err != nil {
//			log.Fatalf("Failed to open service backends: %v", err)
//		}
//
//		// ServerKeepalive accepts the client's pings during a long Claim.
//		s := grpc.NewServer(eqgrpc.ServerKeepalive())
//		pb.RegisterEntroQServer(s, svc)
//		s.Serve(listener)
//	}
package eqsvcgrpc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/authn"
	"github.com/shiblon/entroq/pkg/authz"
	"github.com/shiblon/entroq/pkg/pbconv"
	"github.com/shiblon/entroq/pkg/queues"
	"github.com/shiblon/entroq/pkg/version"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	protov2 "google.golang.org/protobuf/proto"

	pb "github.com/shiblon/entroq/api"
)

// QSvc is an EntroQServer.
type QSvc struct {
	pb.UnimplementedEntroQServer

	impl *entroq.EntroQ

	mp             metric.MeterProvider
	metricInterval time.Duration

	leaseFloor   time.Duration
	leaseCeiling time.Duration
	leaseClamped metric.Int64Counter

	// guards the cached stats used by the observable gauge callback.
	mu                   sync.Mutex
	lastQueueRefresh     time.Time
	lastQueueStats       map[string]*entroq.QueueStat
	lastNamespaceRefresh time.Time
	lastNamespaceStats   map[string]*entroq.NamespaceStat

	authzHeader string
	an          authn.Authenticator
	az          authz.Authorizer
}

// Option allows QSvc creation options to be defined.
type Option func(*QSvc)

// WithMeterProvider sets the OTel MeterProvider used to emit task and doc metrics.
// Defaults to a noop provider.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(s *QSvc) {
		s.mp = mp
	}
}

// WithMetricInterval sets the minimum time between database queries for queue
// and namespace stats. The observable gauge callback caches results for this
// duration, so frequent Prometheus scrapes do not hammer the database.
// Capped from below at 5 seconds.
func WithMetricInterval(d time.Duration) Option {
	return func(s *QSvc) {
		if d < 5*time.Second {
			d = 5 * time.Second
		}
		s.metricInterval = d
	}
}

// Default bounds a client's requested claim lease is clamped into, for tasks
// and doc sets alike, unless WithClaimLeaseBounds says otherwise.
//
// The floor is the default claim duration, so a client may ask for a LONGER
// lease but never a shorter one. The server dictates lease length because a
// client has no incentive to be careful with it: a lease too short to survive
// its own first renewal churns storage and burns claim counts for every other
// client, not just the one that asked.
//
// The ceiling bounds how long a claim may hold what it took, on its FIRST hold
// only: renewal is a lease change rather than a claim, and is not clamped. A
// holder that keeps renewing is not bounded by it.
const (
	DefaultClaimLeaseFloor   = entroq.DefaultClaimDuration
	DefaultClaimLeaseCeiling = time.Hour
)

// WithClaimLeaseBounds sets the range a client's requested claim lease is
// clamped into, replacing DefaultClaimLeaseFloor and DefaultClaimLeaseCeiling.
// A request naming no duration is left alone for the default to apply; one
// outside the bounds is clamped to the nearer bound and counted, rather than
// refused, because a lease shorter than the floor is a legitimate thing for a
// client to ask when its own deadline is near. Asking for something nobody
// could have meant is refused earlier, by entroq.MaxClaimDuration.
//
// New reports an error if the ceiling is below the floor, or either is not
// positive.
func WithClaimLeaseBounds(floor, ceiling time.Duration) Option {
	return func(s *QSvc) {
		s.leaseFloor, s.leaseCeiling = floor, ceiling
	}
}

// WithAuthorizationHeader sets the name of the header containing an authorization token. Default is "authorization".
func WithAuthorizationHeader(h string) Option {
	return func(s *QSvc) {
		s.authzHeader = h
	}
}

// WithAuthorizer sets the authorization implementation.
func WithAuthorizer(az authz.Authorizer) Option {
	return func(s *QSvc) {
		s.az = az
	}
}

// WithAuthenticator sets the authentication implementation. An authenticator
// and authorizer must be configured together: authentication establishes the
// principal and authorization decides what that principal may do.
func WithAuthenticator(an authn.Authenticator) Option {
	return func(s *QSvc) {
		s.an = an
	}
}

// New creates a new service that exposes gRPC endpoints for task queue access.
func New(ctx context.Context, opener entroq.BackendOpener, opts ...Option) (*QSvc, error) {
	impl, err := entroq.New(ctx, opener)
	if err != nil {
		return nil, fmt.Errorf("eqsvcgrpc backend client: %w", err)
	}

	svc := &QSvc{
		impl:           impl,
		mp:             noop.NewMeterProvider(),
		metricInterval: time.Minute,
		authzHeader:    "authorization",
		leaseFloor:     DefaultClaimLeaseFloor,
		leaseCeiling:   DefaultClaimLeaseCeiling,
	}

	for _, o := range opts {
		o(svc)
	}
	if (svc.an == nil) != (svc.az == nil) {
		_ = impl.Close()
		return nil, fmt.Errorf("eqsvcgrpc authentication and authorization must be configured together")
	}
	if svc.leaseFloor <= 0 || svc.leaseCeiling <= 0 || svc.leaseCeiling < svc.leaseFloor {
		_ = impl.Close()
		return nil, fmt.Errorf("eqsvcgrpc claim lease bounds must be positive with ceiling >= floor, got [%v, %v]", svc.leaseFloor, svc.leaseCeiling)
	}
	if svc.leaseCeiling > entroq.MaxClaimDuration {
		_ = impl.Close()
		return nil, fmt.Errorf("eqsvcgrpc claim lease ceiling %v exceeds the sanity bound %v", svc.leaseCeiling, entroq.MaxClaimDuration)
	}

	if err := svc.initMetrics(); err != nil {
		return nil, fmt.Errorf("eqsvcgrpc init metrics: %w", err)
	}

	// Garbage collection is handled by the backend itself (an always-on internal
	// loop), so the service does not run one.

	return svc, nil
}

// initMetrics registers observable gauges that query queue and namespace stats
// on each collection cycle, caching each result for metricInterval to avoid
// hammering the database on every Prometheus scrape.
func (s *QSvc) initMetrics() error {
	meter := s.mp.Meter("entroq.svc")

	queueGauge, err := meter.Float64ObservableGauge("entroq.queue.size",
		metric.WithDescription("Number of tasks in a named queue, by type."),
	)
	if err != nil {
		return fmt.Errorf("queue size gauge: %w", err)
	}
	namespaceGauge, err := meter.Float64ObservableGauge("entroq.namespace.size",
		metric.WithDescription("Number of docs in a named namespace, by type."),
	)
	if err != nil {
		return fmt.Errorf("namespace size gauge: %w", err)
	}
	if s.leaseClamped, err = meter.Int64Counter("entroq.claim.lease_clamped",
		metric.WithDescription("Claims whose requested lease was clamped into the service's bounds, by kind and reason."),
	); err != nil {
		return fmt.Errorf("lease clamped counter: %w", err)
	}

	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		s.mu.Lock()
		defer s.mu.Unlock()

		if time.Since(s.lastQueueRefresh) >= s.metricInterval || s.lastQueueStats == nil {
			stats, err := s.impl.QueueStats(ctx)
			if err != nil {
				log.Printf("eqsvcgrpc: queue stats for metrics: %v", err)
			} else {
				s.lastQueueStats = foldQueueMetricStats(stats)
				s.lastQueueRefresh = time.Now()
			}
		}
		if s.lastQueueStats != nil {
			s.observeQueueStats(o, queueGauge, s.lastQueueStats)
		}

		if time.Since(s.lastNamespaceRefresh) >= s.metricInterval || s.lastNamespaceStats == nil {
			stats, err := s.impl.NamespaceStats(ctx)
			if err != nil {
				log.Printf("eqsvcgrpc: namespace stats for metrics: %v", err)
			} else {
				s.lastNamespaceStats = foldNamespaceMetricStats(stats)
				s.lastNamespaceRefresh = time.Now()
			}
		}
		if s.lastNamespaceStats != nil {
			s.observeNamespaceStats(o, namespaceGauge, s.lastNamespaceStats)
		}
		return nil
	}, queueGauge, namespaceGauge)

	return err
}

// foldQueueMetricStats aggregates session-scoped queues before they become
// metric label sets. Counts sum across queues; MaxClaims remains a maximum.
func foldQueueMetricStats(stats map[string]*entroq.QueueStat) map[string]*entroq.QueueStat {
	folded := make(map[string]*entroq.QueueStat, len(stats))
	for name, stat := range stats {
		name = queues.FoldPathParam(name, "sess")
		aggregate := folded[name]
		if aggregate == nil {
			folded[name] = &entroq.QueueStat{
				Name:      name,
				Size:      stat.Size,
				Claimed:   stat.Claimed,
				Available: stat.Available,
				Future:    stat.Future,
				MaxClaims: stat.MaxClaims,
			}
			continue
		}
		aggregate.Size += stat.Size
		aggregate.Claimed += stat.Claimed
		aggregate.Available += stat.Available
		aggregate.Future += stat.Future
		if stat.MaxClaims > aggregate.MaxClaims {
			aggregate.MaxClaims = stat.MaxClaims
		}
	}
	return folded
}

// foldNamespaceMetricStats aggregates session-scoped doc namespaces before
// they become metric label sets.
func foldNamespaceMetricStats(stats map[string]*entroq.NamespaceStat) map[string]*entroq.NamespaceStat {
	folded := make(map[string]*entroq.NamespaceStat, len(stats))
	for name, stat := range stats {
		name = queues.FoldPathParam(name, "sess")
		aggregate := folded[name]
		if aggregate == nil {
			folded[name] = &entroq.NamespaceStat{
				Name:    name,
				Size:    stat.Size,
				Claimed: stat.Claimed,
			}
			continue
		}
		aggregate.Size += stat.Size
		aggregate.Claimed += stat.Claimed
	}
	return folded
}

// observeQueueStats reports queue stat values to the OTel observer. Must be
// called with s.mu held.
func (s *QSvc) observeQueueStats(o metric.Observer, gauge metric.Float64ObservableGauge, stats map[string]*entroq.QueueStat) {
	for name, stat := range stats {
		l1, l2, l3 := queues.PathLabels(name)

		base := []attribute.KeyValue{
			attribute.String("queue", name),
			attribute.String("l1", l1),
			attribute.String("l2", l2),
			attribute.String("l3", l3),
		}

		for typ, val := range map[string]int{
			"total":     stat.Size,
			"claimed":   stat.Claimed,
			"available": stat.Available,
			"maxClaims": stat.MaxClaims,
		} {
			attrs := append(base, attribute.String("type", typ))
			o.ObserveFloat64(gauge, float64(val), metric.WithAttributes(attrs...))
		}
	}
}

// observeNamespaceStats reports doc namespace stat values to the OTel observer.
// Must be called with s.mu held.
func (s *QSvc) observeNamespaceStats(o metric.Observer, gauge metric.Float64ObservableGauge, stats map[string]*entroq.NamespaceStat) {
	for name, stat := range stats {
		l1, l2, l3 := queues.PathLabels(name)

		base := []attribute.KeyValue{
			attribute.String("doc_namespace", name),
			attribute.String("l1", l1),
			attribute.String("l2", l2),
			attribute.String("l3", l3),
		}

		for typ, val := range map[string]int{
			"total":   stat.Size,
			"claimed": stat.Claimed,
		} {
			attrs := append(base, attribute.String("type", typ))
			o.ObserveFloat64(gauge, float64(val), metric.WithAttributes(attrs...))
		}
	}
}

// Close closes the backend connections.
func (s *QSvc) Close() error {
	var closeErrors []error
	if s.az != nil {
		if err := s.az.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close authorizer: %w", err))
		}
	}
	if s.an != nil {
		if err := s.an.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close authenticator: %w", err))
		}
	}
	if err := s.impl.Close(); err != nil {
		closeErrors = append(closeErrors, fmt.Errorf("close backend: %w", err))
	}
	return errors.Join(closeErrors...)
}

// Authorize attempts to authorize an action.
func (s *QSvc) Authorize(ctx context.Context, req *authz.Request) error {
	if s.az == nil {
		return nil
	}
	principal, err := s.an.Authenticate(ctx, authn.NewHeaderCredentials(s.authzToken(ctx)))
	if err != nil {
		var authErr *authn.Error
		if !errors.As(err, &authErr) {
			return status.Error(codes.Internal, "authentication failed")
		}
		switch authErr.Kind {
		case authn.InvalidCredentials:
			return status.Error(codes.Unauthenticated, authErr.Error())
		case authn.AuthenticationUnavailable:
			return status.Error(codes.Unavailable, authErr.Error())
		default:
			return status.Error(codes.Internal, "authentication failed")
		}
	}
	if principal == nil {
		return status.Error(codes.Internal, "authentication returned no principal")
	}
	req.Principal = principal

	// Most of this is error formatting to provide structured things that can
	// be unpacked and round-tripped through the grpc transport.
	if err := s.az.Authorize(ctx, req); err != nil {
		var details []proto.Message
		authzErr := new(authz.AuthzError)
		if !errors.As(err, &authzErr) {
			return status.New(codes.PermissionDenied, fmt.Sprintf("unknown authz error: %v", err)).Err()
		}

		for _, msg := range authzErr.Errors {
			details = append(details, &pb.AuthzDep{
				Actions: []pb.ActionType{pb.ActionType_DETAIL},
				Msg:     msg,
			})
		}
		for _, q := range authzErr.Failed {
			var actions []pb.ActionType
			for _, a := range q.Actions {
				switch a {
				case "READ":
					actions = append(actions, pb.ActionType_READ)
				case "INSERT":
					actions = append(actions, pb.ActionType_INSERT)
				case "CLAIM":
					actions = append(actions, pb.ActionType_CLAIM)
				case "DELETE":
					actions = append(actions, pb.ActionType_DELETE)
				case "CHANGE":
					actions = append(actions, pb.ActionType_CHANGE)
				default:
					details = append(details, &pb.AuthzDep{
						Actions: []pb.ActionType{pb.ActionType_DETAIL},
						Exact:   fmt.Sprintf("WARNING: Unknown action %q", a),
					})
				}
			}
			details = append(details, &pb.AuthzDep{
				Actions: actions,
				Exact:   q.Exact,
				Prefix:  q.Prefix,
			})
		}
		stat, sErr := status.New(codes.PermissionDenied, "queue action denied").WithDetails(details...)
		if sErr != nil {
			return status.New(codes.PermissionDenied, fmt.Sprintf("queue action denied, unable to add details to %v: %v", err, sErr)).Err()
		}
		return stat.Err()
	}

	return nil
}

func autoCodeErrorf(format string, vals ...any) error {
	err := fmt.Errorf(format, vals...)
	if entroq.IsTimeout(err) {
		return status.New(codes.DeadlineExceeded, err.Error()).Err()
	}
	if entroq.IsCanceled(err) {
		return status.New(codes.Canceled, err.Error()).Err()
	}
	if entroq.IsInvalidArgument(err) {
		return status.New(codes.InvalidArgument, err.Error()).Err()
	}
	// An error that already carries a status, such as one passed through from
	// a remote backend, keeps its code; anything else is the server's failure.
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.New(codes.Internal, err.Error()).Err()
}

func codeErrorf(code codes.Code, format string, vals ...any) error {
	return status.New(code, fmt.Errorf(format, vals...).Error()).Err()
}

// authzToken gets the Authorization token from headers (grpc context) if present, otherwise blank.
func (s *QSvc) authzToken(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md[s.authzHeader]
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

func (s *QSvc) newAuthzRequest(_ context.Context) *authz.Request {
	return new(authz.Request)
}

func (s *QSvc) claimAuthz(ctx context.Context, req *pb.ClaimRequest) *authz.Request {
	authReq := s.newAuthzRequest(ctx)
	authReq.ClaimantId = req.ClaimantId

	for _, q := range req.GetQueues() {
		authReq.Queues = append(authReq.Queues, &authz.Queue{
			Exact:   q,
			Actions: []authz.Action{authz.Claim},
		})
	}
	return authReq
}

func (s *QSvc) tasksAuthz(ctx context.Context, req *pb.TasksRequest) *authz.Request {
	authReq := s.newAuthzRequest(ctx)
	authReq.ClaimantId = req.ClaimantId
	authReq.Queues = append(authReq.Queues, &authz.Queue{
		Exact:   req.Queue,
		Actions: []authz.Action{authz.Read},
	})
	return authReq
}

// modifyAuthz builds the authorization request for a Modify. Every task op
// contributes a (queue, action) requirement and every doc op a (namespace,
// action) one, so the request represents exactly the operations' claimed
// targets. Whether a caller lied about a target (named a queue/namespace the
// task/doc does not actually live in) is caught later by the backend, which
// binds each operation to the target's real location.
//
// An empty target is a fail-closed error: there is no way to be granted rights
// on an unnamed queue or namespace, so an operation that names one can never be
// authorized. (A change with an empty destination is not this case; the
// destination defaults to the source, which is the "no move" reading.)
func (s *QSvc) modifyAuthz(ctx context.Context, req *pb.ModifyRequest) (*authz.Request, error) {
	authReq := s.newAuthzRequest(ctx)
	authReq.ClaimantId = req.ClaimantId

	var empty bool
	q := func(name string, a authz.Action) {
		if name == "" {
			empty = true
			return
		}
		authReq.Queues = append(authReq.Queues, &authz.Queue{Exact: name, Actions: []authz.Action{a}})
	}
	n := func(name string, a authz.Action) {
		if name == "" {
			empty = true
			return
		}
		authReq.Namespaces = append(authReq.Namespaces, &authz.Namespace{Exact: name, Actions: []authz.Action{a}})
	}
	// change emits the authz requirement(s) for a task change via add. The source
	// (from) queue is always required. An empty destination (to), or one equal to
	// the source, means "no move" and requires Change on the source; a different,
	// non-empty destination is a move, needing Delete on the source and Insert on
	// the destination. Docs do not use this: they cannot move namespaces, so a doc
	// change is authorized in place (see the DocChanges loop below).
	change := func(from, to string, add func(string, authz.Action)) {
		if to == "" || to == from {
			add(from, authz.Change)
			return
		}
		add(from, authz.Delete)
		add(to, authz.Insert)
	}

	for _, ins := range req.Inserts {
		q(ins.Queue, authz.Insert)
	}
	for _, chg := range req.Changes {
		change(chg.GetOldId().GetQueue(), chg.GetNewData().GetQueue(), q)
	}
	for _, del := range req.Deletes {
		q(del.Queue, authz.Delete)
	}
	for _, dep := range req.Depends {
		q(dep.Queue, authz.Read)
	}
	for _, di := range req.DocInserts {
		n(di.Namespace, authz.Insert)
	}
	for _, dc := range req.DocChanges {
		// Docs do not move between namespaces, so a doc change is always in place:
		// authorize Change on the doc's namespace. A cross-namespace change is
		// rejected in Modify (which runs even when no authorizer is configured).
		n(dc.GetOldId().GetNamespace(), authz.Change)
	}
	for _, dd := range req.DocDeletes {
		n(dd.Namespace, authz.Delete)
	}
	for _, ddep := range req.DocDepends {
		n(ddep.Namespace, authz.Read)
	}

	if empty {
		return nil, fmt.Errorf("modification names an empty queue or namespace, which can never be authorized")
	}
	return authReq, nil
}

// protocolHeaders are the response headers every RPC sends: the protocols
// this server serves, from which a client chooses, and its release.
var protocolHeaders = metadata.Pairs(
	version.ProtocolHeader, version.FormatProtocols(version.ServedProtocols),
	version.VersionHeader, version.Version,
)

// negotiate sends the protocol headers on a unary RPC, and returns the
// protocol the request declares, under which the server reads it. A request
// that declares none is protocol 1, as every client before the header is. One
// declaring a protocol this server does not serve is refused, naming what to
// upgrade, and so is one carrying fields this server does not know (see
// checkKnown). Sending the headers does nothing for a caller not over gRPC;
// the JSON handler sends them itself.
func negotiate(ctx context.Context, req protov2.Message) (int32, error) {
	_ = grpc.SetHeader(ctx, protocolHeaders)
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get(version.ProtocolHeader)
	if len(vals) == 0 {
		return 1, checkKnown(req, 1)
	}
	ps, err := version.ParseProtocols(vals[0])
	if err != nil || len(ps) != 1 {
		return 0, codeErrorf(codes.InvalidArgument, "a request declares one protocol in %s, got %q", version.ProtocolHeader, vals[0])
	}
	p := ps[0]
	if !version.Serves(p) {
		upgrade := "the client"
		if p > version.Protocol {
			upgrade = "the server"
		}
		return 0, codeErrorf(codes.Unimplemented, "this server serves EntroQ protocols %s, not %d: upgrade %s",
			version.FormatProtocols(version.ServedProtocols), p, upgrade)
	}
	if err := checkKnown(req, p); err != nil {
		return 0, err
	}
	return p, nil
}

// Claim is the blocking version of TryClaim.
func (s *QSvc) Claim(ctx context.Context, req *pb.ClaimRequest) (*pb.ClaimResponse, error) {
	if _, err := negotiate(ctx, req); err != nil {
		return nil, err
	}
	pollTime := time.Duration(0)
	if req.PollMs > 0 {
		pollTime = time.Duration(req.PollMs) * time.Millisecond
	}
	opts := append(s.claimOpts(ctx, req), entroq.ClaimPollTime(pollTime))
	if err := entroq.NewClaimQuery(opts...).Validate(); err != nil {
		return nil, autoCodeErrorf("claim: %w", err)
	}
	if err := s.Authorize(ctx, s.claimAuthz(ctx, req)); err != nil {
		return nil, err // don't wrap, has status codes
	}

	// Block until a task is available or the caller's context ends, re-checking
	// the store every pollTime (so time-passage-ready tasks are caught). The
	// client holds this open on a single RPC with no per-attempt deadline; a dead
	// connection is surfaced by client keepalive, not by racing claim delivery
	// with a cancel. See pkg/backend/eqgrpc backend.Claim.
	task, err := s.impl.Claim(ctx, opts...)
	if err != nil {
		return nil, autoCodeErrorf("eqsvcgrpc claim: %w", err)
	}
	if task == nil {
		return new(pb.ClaimResponse), nil
	}
	pt, err := pbconv.TaskToProto(task)
	if err != nil {
		return nil, autoCodeErrorf("claim task proto: %w", err)
	}
	return &pb.ClaimResponse{Task: pt}, nil
}

// TryClaim attempts to claim a task, returning immediately. If no tasks are
// available, it returns a nil response and a nil error.
//
// If req.Wait is present, TryClaim may not return immediately, but may hold
// onto the connection until either the context expires or a task becomes
// available to claim. Callers can check for context cancelation codes to know
// that this has happened, and may opt to immediately re-send the request.
func (s *QSvc) TryClaim(ctx context.Context, req *pb.ClaimRequest) (*pb.ClaimResponse, error) {
	if _, err := negotiate(ctx, req); err != nil {
		return nil, err
	}
	opts := s.claimOpts(ctx, req)
	if err := entroq.NewClaimQuery(opts...).Validate(); err != nil {
		return nil, autoCodeErrorf("try claim: %w", err)
	}
	if err := s.Authorize(ctx, s.claimAuthz(ctx, req)); err != nil {
		return nil, err // don't wrap, has status codes
	}

	task, err := s.impl.TryClaim(ctx, opts...)
	if err != nil {
		return nil, autoCodeErrorf("try claim: %w", err)
	}
	if task == nil {
		return new(pb.ClaimResponse), nil
	}
	pt, err := pbconv.TaskToProto(task)
	if err != nil {
		return nil, autoCodeErrorf("try-claim task proto: %w", err)
	}
	return &pb.ClaimResponse{Task: pt}, nil
}

// claimOpts are the claim options a request asks for. The claimant is the
// request's, never the service's own: an empty one fails the claim's check.
func (s *QSvc) claimOpts(ctx context.Context, req *pb.ClaimRequest) []entroq.ClaimOpt {
	return []entroq.ClaimOpt{
		entroq.From(req.Queues...),
		entroq.ClaimFor(s.clampLease(ctx, "task", time.Duration(req.DurationMs)*time.Millisecond)),
		entroq.WithClaimant(req.ClaimantId),
	}
}

// clampLease brings a client's requested lease within the service's bounds,
// counting any clamp it makes so an operator can see a client asking for
// something it will not get. A non-positive duration means the request named
// none, and comes back unchanged so the caller's own default applies.
//
// It clamps rather than refuses. A lease below the floor is a reasonable thing
// to ask for -- a worker whose own deadline is near wants its doc sets to
// expire with it -- it just cannot have it, because a lease too short to
// survive its first renewal costs every other client. A lease nobody could
// have meant was already refused by entroq.MaxClaimDuration.
func (s *QSvc) clampLease(ctx context.Context, kind string, d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	bounded, reason := d, ""
	switch {
	case d < s.leaseFloor:
		bounded, reason = s.leaseFloor, "below_floor"
	case d > s.leaseCeiling:
		bounded, reason = s.leaseCeiling, "above_ceiling"
	}
	if reason != "" {
		s.countLeaseClamp(ctx, kind, reason)
	}
	return bounded
}

// countLeaseClamp records that a claim did not get the lease it asked for.
// Attributes stay low-cardinality on purpose: a claimant would name the client
// at fault, but claimant IDs are per-consumer and would make this unusable as
// a time series.
func (s *QSvc) countLeaseClamp(ctx context.Context, kind, reason string) {
	s.leaseClamped.Add(ctx, 1, metric.WithAttributes(
		attribute.String("kind", kind),
		attribute.String("reason", reason),
	))
}

// resolveSetLease decides how long a doc claim holds its sets, given the
// duration and the absolute time a request may each name. Protocol 2 allows
// both, which protocol 1 refused; an absolute time wins when one is given, and
// the duration is what a request falls back on when it names no time.
//
// Either way the answer is clamped, because a lease too short churns storage
// for every other client and one too long strands what a dead holder was
// holding. A duration is clamped into the service's bounds whole; a time is
// clamped into the same ceiling but to two thirds of the floor.
//
// A time in the past cannot be honored at all and is refused, because it is
// always the caller's mistake and never a race.
//
// A nil result means the request named no lease worth passing on, and
// entroq.ClaimDocs fills in DefaultClaimDuration.
func (s *QSvc) resolveSetLease(ctx context.Context, now time.Time, duration time.Duration, at time.Time) (entroq.DocClaimArg, error) {
	if at.IsZero() {
		if duration = s.clampLease(ctx, "doc", duration); duration <= 0 {
			return nil, nil
		}
		return entroq.ClaimingSetsFor(duration), nil
	}
	if !at.After(now) {
		return nil, codeErrorf(codes.InvalidArgument, "claim docs: at_ms names %v, which is not in the future", at)
	}
	// A named time may reach below the lease floor, down to one renewal's worth
	// of it, which is what lets a claim expire in step with a task whose lease
	// is part spent -- the reason to name a time rather than a duration.
	minHold := entroq.RenewalDurationFor(s.leaseFloor)
	switch {
	case at.Before(now.Add(minHold)):
		s.countLeaseClamp(ctx, "doc", "below_floor")
		return entroq.ClaimingSetsUntil(now.Add(minHold)), nil
	case at.After(now.Add(s.leaseCeiling)):
		s.countLeaseClamp(ctx, "doc", "above_ceiling")
		return entroq.ClaimingSetsUntil(now.Add(s.leaseCeiling)), nil
	}
	return entroq.ClaimingSetsUntil(at), nil
}

// Modify attempts to make the specified modification from the given
// ModifyRequest. If all goes well, it returns a ModifyResponse. If the
// modification fails due to a dependency error (one of the specified tasks was
// not present), the gRPC status mechanism is invoked to return a status with
// the details slice containing *pb.ModifyDep values. These could be used to
// reconstruct an entroq.DependencyError, or directly to find out which IDs
// caused the dependency failure. Code UNKNOWN is returned on other errors.
func (s *QSvc) Modify(ctx context.Context, req *pb.ModifyRequest) (*pb.ModifyResponse, error) {
	protocol, err := negotiate(ctx, req)
	if err != nil {
		return nil, err
	}
	// Check the modification's shape before authorizing it, with the same
	// check every backend runs, so a malformed request is InvalidArgument
	// whether or not an authorizer is configured.
	modArgs, err := pbconv.ModifyArgsFromProto(req, protocol)
	if err != nil {
		var inv *pbconv.InvalidRequestError
		if errors.As(err, &inv) {
			return nil, codeErrorf(codes.InvalidArgument, "%v", err)
		}
		var uns *pbconv.UnsupportedRequestError
		if errors.As(err, &uns) {
			return nil, codeErrorf(codes.Unimplemented, "%v", err)
		}
		return nil, autoCodeErrorf("modify: %w", err)
	}
	if err := entroq.NewModification(req.ClaimantId, modArgs...).EnsureModifyKeys(); err != nil {
		return nil, autoCodeErrorf("modify: %w", err)
	}

	// Authorization is enforced only when an authorizer is configured (admins may
	// run open). A modification naming an empty queue or namespace fails the
	// check above; modifyAuthz still refuses one, as a backstop.
	if s.az != nil {
		authReq, err := s.modifyAuthz(ctx, req)
		if err != nil {
			return nil, codeErrorf(codes.PermissionDenied, "modify authz: %v", err)
		}
		if err := s.Authorize(ctx, authReq); err != nil {
			return nil, err // don't wrap, has status codes
		}
	}

	resp, err := s.impl.Modify(ctx, modArgs...)
	if err != nil {
		if depErr, ok := entroq.AsDependency(err); ok {
			details := depDetails(depErr)
			stat, sErr := status.New(codes.NotFound, "modification dependency error").WithDetails(details...)
			if sErr != nil {
				return nil, codeErrorf(codes.NotFound, "dependency failed, and failed to add details %v: %w", err, sErr)
			}
			return nil, stat.Err()
		}
		return nil, autoCodeErrorf("modification failed: %w", err)
	}
	// Assemble the response.
	pbResp := new(pb.ModifyResponse)
	for _, task := range resp.InsertedTasks {
		pt, err := pbconv.TaskToProto(task)
		if err != nil {
			return nil, autoCodeErrorf("modify inserted task proto: %w", err)
		}
		pbResp.Inserted = append(pbResp.Inserted, pt)
	}
	for _, task := range resp.ChangedTasks {
		pt, err := pbconv.TaskToProto(task)
		if err != nil {
			return nil, autoCodeErrorf("modify changed task proto: %w", err)
		}
		pbResp.Changed = append(pbResp.Changed, pt)
	}
	for _, d := range resp.InsertedDocs {
		pd, err := pbconv.DocToProto(d)
		if err != nil {
			return nil, autoCodeErrorf("modify inserted doc proto: %w", err)
		}
		pbResp.InsertedDocs = append(pbResp.InsertedDocs, pd)
	}
	for _, d := range resp.ChangedDocs {
		pd, err := pbconv.DocToProto(d)
		if err != nil {
			return nil, autoCodeErrorf("modify changed doc proto: %w", err)
		}
		pbResp.ChangedDocs = append(pbResp.ChangedDocs, pd)
	}
	// A doc set whose lease changed comes back as a Doc with no ID.
	for _, g := range resp.ChangedSets {
		pbResp.ChangedDocs = append(pbResp.ChangedDocs, pbconv.DocSetToProto(g))
	}
	return pbResp, nil
}

func (s *QSvc) Tasks(ctx context.Context, req *pb.TasksRequest) (*pb.TasksResponse, error) {
	if _, err := negotiate(ctx, req); err != nil {
		return nil, err
	}
	if err := (&entroq.TasksQuery{Queue: req.Queue, IDs: req.TaskId}).Validate(); err != nil {
		return nil, autoCodeErrorf("tasks: %w", err)
	}
	if err := s.Authorize(ctx, s.tasksAuthz(ctx, req)); err != nil {
		return nil, err // don't wrap, has status codes
	}

	// Claimant will only really be a filter if it is nonzero.
	// Task IDs will only be used as a filter if non-empty.
	opts := []entroq.TasksOpt{
		entroq.ClaimedBy(req.ClaimantId),
		entroq.WithTaskID(req.TaskId...),
		entroq.LimitTasks(int(req.Limit)),
	}
	if req.OmitValues {
		opts = append(opts, entroq.OmitValues())
	}
	tasks, err := s.impl.Tasks(ctx, req.Queue, opts...)
	if err != nil {
		return nil, autoCodeErrorf("failed to get tasks: %w", err)
	}
	resp := new(pb.TasksResponse)
	for _, task := range tasks {
		// Backends already bind task IDs to their queue, but the service must
		// not depend on that to keep a caller out of queues it cannot read.
		if req.Queue != "" && task.Queue != req.Queue {
			continue
		}
		pt, err := pbconv.TaskToProto(task)
		if err != nil {
			return nil, autoCodeErrorf("tasks task proto: %w", err)
		}
		resp.Tasks = append(resp.Tasks, pt)
	}
	return resp, nil
}

func (s *QSvc) StreamTasks(req *pb.TasksRequest, stream pb.EntroQ_StreamTasksServer) error {
	_ = stream.SetHeader(protocolHeaders)
	resp, err := s.Tasks(stream.Context(), req)
	if err != nil {
		return autoCodeErrorf("get tasks to stream: %w", err)
	}

	// Note, we send a full TasksResponse each time because there might be
	// additional metadata added to that response later. This is more
	// future-proof.
	for _, task := range resp.Tasks {
		if err := stream.Send(&pb.TasksResponse{Tasks: []*pb.Task{task}}); err != nil {
			return autoCodeErrorf("send stream tasks: %w", err)
		}
	}
	return nil
}

// Queues returns a mapping from queue names to queue sizes.
//
// TODO(listing-authz): currently UNGATED even when an authorizer is configured.
// Enumerating queue names/prefixes is a listing capability we intend to gate on
// a dedicated authz action (distinct from Read on task content). That lands in a
// follow-up because it also requires an authz-policy/CRD schema change.
func (s *QSvc) Queues(ctx context.Context, req *pb.QueuesRequest) (*pb.QueuesResponse, error) {
	if _, err := negotiate(ctx, req); err != nil {
		return nil, err
	}
	queueMap, err := s.impl.Queues(ctx,
		entroq.MatchPrefix(req.MatchPrefix...),
		entroq.MatchExact(req.MatchExact...),
		entroq.LimitQueues(int(req.Limit)))
	if err != nil {
		return nil, autoCodeErrorf("failed to get queues: %w", err)
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

// QueueStats returns a mapping from queue names to queue stats.
//
// TODO(listing-authz): currently UNGATED. Same listing capability as Queues;
// see that method. Gated in the follow-up.
func (s *QSvc) QueueStats(ctx context.Context, req *pb.QueuesRequest) (*pb.QueuesResponse, error) {
	if _, err := negotiate(ctx, req); err != nil {
		return nil, err
	}
	queueMap, err := s.impl.QueueStats(ctx,
		entroq.MatchPrefix(req.MatchPrefix...),
		entroq.MatchExact(req.MatchExact...),
		entroq.LimitQueues(int(req.Limit)))
	if err != nil {
		return nil, autoCodeErrorf("failed to get queues: %w", err)
	}
	resp := new(pb.QueuesResponse)
	for _, stat := range queueMap {
		resp.Queues = append(resp.Queues, &pb.QueueStats{
			Name:         stat.Name,
			NumTasks:     int32(stat.Size),
			NumClaimed:   int32(stat.Claimed),
			NumAvailable: int32(stat.Available),
			NumFuture:    int32(stat.Future),
			MaxClaims:    int32(stat.MaxClaims),
		})
	}
	return resp, nil
}

// Time returns the current time in milliseconds since the Epoch.
//
// Intentionally UNAUTHENTICATED: it is the server clock, carries no queue or
// task data, and clients need it to reason about arrival times.
func (s *QSvc) Time(ctx context.Context, req *pb.TimeRequest) (*pb.TimeResponse, error) {
	if _, err := negotiate(ctx, req); err != nil {
		return nil, err
	}
	return &pb.TimeResponse{TimeMs: pbconv.ToMS(time.Now().UTC())}, nil
}

// depDetails renders a DependencyError as the gRPC status detail messages naming
// which dependencies failed. It adapts the shared, transport-neutral pbconv
// renderer (which returns []*pb.ModifyDep) to the []proto.Message that
// status.WithDetails wants, so the gRPC layer owns the widening and pbconv stays
// gRPC-agnostic.
func depDetails(depErr *entroq.DependencyError) []proto.Message {
	deps := pbconv.DependencyErrorDetails(depErr)
	details := make([]proto.Message, len(deps))
	for i, d := range deps {
		details[i] = d
	}
	return details
}

// Docs returns a listing of docs matching the given query.
func (s *QSvc) Docs(ctx context.Context, req *pb.DocsRequest) (*pb.DocsResponse, error) {
	if _, err := negotiate(ctx, req); err != nil {
		return nil, err
	}
	q := req.GetQuery()
	dq := &entroq.DocQuery{
		Namespace:  q.GetNamespace(),
		IDs:        q.GetIds(),
		KeyExact:   q.GetKeyExact(),
		KeyStart:   q.GetKeyStart(),
		KeyEnd:     q.GetKeyEnd(),
		Limit:      int(q.GetLimit()),
		OmitValues: q.GetOmitValues(),
	}
	if err := dq.Validate(); err != nil {
		return nil, autoCodeErrorf("docs: %w", err)
	}
	// Reading doc content is gated on Read for the namespace (unlike queue/
	// namespace metadata, which is open). Enforced only when an authorizer is set.
	if s.az != nil {
		authReq := s.newAuthzRequest(ctx)
		authReq.Namespaces = append(authReq.Namespaces, &authz.Namespace{
			Exact:   q.GetNamespace(),
			Actions: []authz.Action{authz.Read},
		})
		if err := s.Authorize(ctx, authReq); err != nil {
			return nil, err // don't wrap, has status codes
		}
	}
	docs, err := s.impl.Docs(ctx, dq)
	if err != nil {
		return nil, autoCodeErrorf("docs: %w", err)
	}
	resp := new(pb.DocsResponse)
	for _, d := range docs {
		pd, err := pbconv.DocToProto(d)
		if err != nil {
			return nil, autoCodeErrorf("docs doc proto: %w", err)
		}
		resp.Docs = append(resp.Docs, pd)
	}
	return resp, nil
}

// NamespaceStats returns statistics for doc namespaces matching the query.
//
// TODO(listing-authz): currently UNGATED. Enumerating namespace names/prefixes
// is the doc-side analog of Queues listing and will be gated on the same
// dedicated listing action in the follow-up (needs an authz-policy/CRD change).
// Doc content (Docs) is already gated.
func (s *QSvc) NamespaceStats(ctx context.Context, req *pb.NamespacesRequest) (*pb.NamespacesResponse, error) {
	if _, err := negotiate(ctx, req); err != nil {
		return nil, err
	}
	nsMap, err := s.impl.NamespaceStats(ctx,
		entroq.MatchPrefix(req.MatchPrefix...),
		entroq.MatchExact(req.MatchExact...),
		entroq.WithLimit(int(req.Limit)))
	if err != nil {
		return nil, autoCodeErrorf("namespace stats: %w", err)
	}
	resp := new(pb.NamespacesResponse)
	for _, stat := range nsMap {
		resp.Namespaces = append(resp.Namespaces, &pb.NamespaceStat{
			Name:       stat.Name,
			NumDocs:    int32(stat.Size),
			NumClaimed: int32(stat.Claimed),
		})
	}
	return resp, nil
}

// ClaimDocs atomically claims a set of docs matching the given query.
// Returns a NotFound status with ModifyDep details if any docs are missing or
// already claimed.
func (s *QSvc) ClaimDocs(ctx context.Context, req *pb.ClaimDocsRequest) (*pb.ClaimDocsResponse, error) {
	protocol, err := negotiate(ctx, req)
	if err != nil {
		return nil, err
	}
	cq := req.GetClaimQuery()
	// The claimant is the request's: the service's client would otherwise fill
	// in its own for an empty one, so check the claim as received. At protocol
	// 2 the sets name what to claim, and the namespace and key are ignored.
	args := []entroq.DocClaimArg{entroq.ClaimingSetsAs(cq.GetClaimant())}
	if sets := cq.GetSets(); len(sets) > 0 {
		if protocol < 2 {
			return nil, codeErrorf(codes.InvalidArgument, "claim docs: sets are protocol 2, and the request declares protocol %d", protocol)
		}
		for _, sc := range sets {
			key, ok := sc.GetSet().GetRef().(*pb.DocID_Key)
			if !ok {
				return nil, codeErrorf(codes.InvalidArgument, "claim docs: a doc set is named by its key, not a doc ID")
			}
			set := entroq.ClaimKey(sc.GetSet().GetNamespace(), key.Key)
			if sc.GetOmitMembers() {
				set = set.WithoutMembers()
			}
			args = append(args, set)
		}
	} else {
		args = append(args, entroq.ClaimKey(cq.GetNamespace(), cq.GetKey()))
	}
	if cq.GetAtMs() != 0 && protocol < 2 {
		return nil, codeErrorf(codes.InvalidArgument, "claim docs: at_ms is protocol 2, and the request declares protocol %d", protocol)
	}
	lease, err := s.resolveSetLease(ctx, entroq.ProcessTime(),
		time.Duration(cq.GetDurationMs())*time.Millisecond,
		pbconv.FromMSOrUnset(cq.GetAtMs()),
	)
	if err != nil {
		return nil, err
	}
	if lease != nil {
		args = append(args, lease)
	}
	dc := entroq.NewDocClaim(args...)
	if err := dc.Validate(); err != nil {
		return nil, autoCodeErrorf("claim docs: %w", err)
	}
	// Claiming a doc set is gated on Claim for its namespace. Enforced only
	// when an authorizer is set.
	if s.az != nil {
		authReq := s.newAuthzRequest(ctx)
		authReq.ClaimantId = cq.GetClaimant()
		var named []string
		for _, set := range dc.Sets {
			if slices.Contains(named, set.Namespace) {
				continue
			}
			named = append(named, set.Namespace)
			authReq.Namespaces = append(authReq.Namespaces, &authz.Namespace{
				Exact:   set.Namespace,
				Actions: []authz.Action{authz.Claim},
			})
		}
		if err := s.Authorize(ctx, authReq); err != nil {
			return nil, err // don't wrap, has status codes
		}
	}
	claimed, err := s.impl.ClaimDocs(ctx, args...)
	if err != nil {
		if depErr, ok := entroq.AsDependency(err); ok {
			details := depDetails(depErr)
			stat, sErr := status.New(codes.NotFound, "claim docs dependency error").WithDetails(details...)
			if sErr != nil {
				return nil, codeErrorf(codes.NotFound, "claim docs dependency failed, unable to add details %v: %w", err, sErr)
			}
			return nil, stat.Err()
		}
		return nil, autoCodeErrorf("claim docs: %w", err)
	}
	resp := new(pb.ClaimDocsResponse)
	for _, g := range claimed {
		resp.Sets = append(resp.Sets, pbconv.DocSetToProto(g))
		for _, d := range g.Docs {
			pd, err := pbconv.DocToProto(d)
			if err != nil {
				return nil, autoCodeErrorf("claim docs doc proto: %w", err)
			}
			resp.Docs = append(resp.Docs, pd)
		}
	}
	return resp, nil
}
