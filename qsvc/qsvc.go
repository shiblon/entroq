// Package qsvc contains the service implementation for registering with gRPC.
// This provides the service that can be registered with a grpc.Server:
//
// 	import (
// 		"context"
// 		"log"
// 		"net"
//
// 		"entrogo.com/entroq/pg"
// 		"entrogo.com/entroq/qsvc"
// 		pb "entrogo.com/entroq/proto"
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
package qsvc // import "entrogo.com/entroq/qsvc"

import (
	"context"
	"fmt"
	"log"
	"time"

	"entrogo.com/entroq"
	"entrogo.com/entroq/pkg/authz"
	"github.com/golang/protobuf/proto"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "entrogo.com/entroq/proto"
)

const (
	// MetricNS is the prometheus namespace for all metrics for this module.
	MetricNS = "entroq"
)

var (
	metricQueueSize = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: MetricNS,
			Subsystem: "queue",
			Name:      "size",
			Help:      "Number of tasks in named queue.",
		},
		[]string{"name", "type"},
	)
)

// QSvc is an EntroQServer.
type QSvc struct {
	pb.UnimplementedEntroQServer

	impl *entroq.EntroQ

	metricCancel    context.CancelFunc
	metricNamespace string
	metricInterval  time.Duration

	authzHeader string
	az          authz.Authorizer
}

// Option allows QSvc creation options to be defined.
type Option func(*QSvc)

// WithMetricInterval sets the interval between (potentially expensive) queue
// stats requests for the purpose of providing metrics. It cannot be set to
// less than 1 minute.
func WithMetricInterval(d time.Duration) Option {
	return func(s *QSvc) {
		if d < time.Minute {
			d = time.Minute
		}
		s.metricInterval = d
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

// New creates a new service that exposes gRPC endpoints for task queue access.
func New(ctx context.Context, opener entroq.BackendOpener, opts ...Option) (*QSvc, error) {
	impl, err := entroq.New(ctx, opener)
	if err != nil {
		return nil, errors.Wrap(err, "qsvc backend client")
	}

	// Note that you should *never* use a background context like this unless
	// it is guaranteed to be exclusively used for a background process. That
	// is the (rare) case here.
	mctx, mcancel := context.WithCancel(context.Background())

	svc := &QSvc{
		impl:           impl,
		metricCancel:   mcancel,
		metricInterval: time.Minute,
		authzHeader:    "authorization",
	}

	for _, o := range opts {
		o(svc)
	}

	go func() {
		for {
			if err := svc.RefreshMetrics(mctx); err != nil {
				log.Printf("Error refreshing metrics: %v", err)
			}
			select {
			case <-mctx.Done():
				return
			case <-time.After(svc.metricInterval):
			}
		}
	}()

	return svc, nil
}

// Close closes the backend connections and flushes the connection free.
func (s *QSvc) Close() error {
	s.metricCancel() // Don't bother waiting for it
	return errors.Wrap(s.impl.Close(), "qsvc close")
}

// Authorize attempts to authorize an action.
func (s *QSvc) Authorize(ctx context.Context, req *authz.Request) error {
	if s.az == nil {
		return nil
	}

	// Most of this is error formatting to provide structured things that can
	// be unpacked and round-tripped through the grpc transport.
	if err := s.az.Authorize(ctx, req); err != nil {
		var details []proto.Message
		// TODO: unwrap after converting to fmt-friendly unwrap instead of pkgerrors unwrap.
		authzErr, ok := err.(*authz.AuthzError)
		if !ok {
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

// RefreshMetrics collects stats and exports them as prometheus metrics.
// Intended to be called periodically in the background. Beware of calling too
// frequently, as this may deny service to users.
func (s *QSvc) RefreshMetrics(ctx context.Context) error {
	stats, err := s.impl.QueueStats(ctx)
	if err != nil {
		return errors.Wrap(err, "collectMetricStats")
	}

	// Clear it out, then repopulate.
	metricQueueSize.Reset()
	for name, stat := range stats {
		metricQueueSize.WithLabelValues(name, "total").Set(float64(stat.Size))
		metricQueueSize.WithLabelValues(name, "claimed").Set(float64(stat.Claimed))
		metricQueueSize.WithLabelValues(name, "available").Set(float64(stat.Available))
		metricQueueSize.WithLabelValues(name, "maxClaims").Set(float64(stat.MaxClaims))
	}

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
		Claims:     t.Claims,
		Value:      t.Value,
		CreatedMs:  toMS(t.Created),
		ModifiedMs: toMS(t.Modified),
	}
}

func wrapErrorf(err error, format string, vals ...interface{}) error {
	if err == nil {
		return nil
	}
	err = errors.Wrapf(err, format, vals...)
	if entroq.IsTimeout(err) {
		return status.New(codes.DeadlineExceeded, err.Error()).Err()
	}
	if entroq.IsCanceled(err) {
		return status.New(codes.Canceled, err.Error()).Err()
	}
	return err
}

func codeErrorf(code codes.Code, err error, format string, vals ...interface{}) error {
	return status.New(code, errors.Wrapf(err, format, vals...).Error()).Err()
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

func (s *QSvc) newAuthzRequest(ctx context.Context) *authz.Request {
	return &authz.Request{
		Authz: authz.NewHeaderAuthorization(s.authzToken(ctx)),
	}
}

func (s *QSvc) claimAuthz(ctx context.Context, req *pb.ClaimRequest) *authz.Request {
	authReq := s.newAuthzRequest(ctx)

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
	authReq.Queues = append(authReq.Queues, &authz.Queue{
		Exact:   req.Queue,
		Actions: []authz.Action{authz.Read},
	})
	return authReq
}

func (s *QSvc) modifyAuthz(ctx context.Context, req *pb.ModifyRequest) *authz.Request {
	authReq := s.newAuthzRequest(ctx)

	for _, ins := range req.Inserts {
		authReq.Queues = append(authReq.Queues, &authz.Queue{
			Exact:   ins.Queue,
			Actions: []authz.Action{authz.Insert},
		})
	}
	for _, chg := range req.Changes {
		oldQueue, newQueue := chg.GetOldId().Queue, chg.GetNewData().Queue
		if oldQueue == newQueue {
			authReq.Queues = append(authReq.Queues, &authz.Queue{
				Exact:   newQueue,
				Actions: []authz.Action{authz.Change},
			})
		} else {
			authReq.Queues = append(authReq.Queues,
				&authz.Queue{
					Exact:   oldQueue,
					Actions: []authz.Action{authz.Delete},
				},
				&authz.Queue{
					Exact:   newQueue,
					Actions: []authz.Action{authz.Insert},
				},
			)
		}
	}
	for _, del := range req.Deletes {
		authReq.Queues = append(authReq.Queues, &authz.Queue{
			Exact:   del.Queue,
			Actions: []authz.Action{authz.Delete},
		})
	}
	for _, dep := range req.Depends {
		authReq.Queues = append(authReq.Queues, &authz.Queue{
			Exact:   dep.Queue,
			Actions: []authz.Action{authz.Read},
		})
	}

	return authReq
}

// Claim is the blocking version of TryClaim.
func (s *QSvc) Claim(ctx context.Context, req *pb.ClaimRequest) (*pb.ClaimResponse, error) {
	if err := s.Authorize(ctx, s.claimAuthz(ctx, req)); err != nil {
		return nil, err // don't wrap, has status codes
	}

	claimant, err := uuid.Parse(req.ClaimantId)
	if err != nil {
		return nil, codeErrorf(codes.InvalidArgument, err, "failed to parse claimant ID")
	}
	duration := time.Duration(req.DurationMs) * time.Millisecond
	pollTime := time.Duration(0)
	if req.PollMs > 0 {
		pollTime = time.Duration(req.PollMs) * time.Millisecond
	}

	task, err := s.impl.Claim(ctx,
		entroq.From(req.Queues...),
		entroq.ClaimFor(duration),
		entroq.ClaimAs(claimant),
		entroq.ClaimPollTime(pollTime))
	if err != nil {
		return nil, wrapErrorf(err, "qsvc claim")
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
	if err := s.Authorize(ctx, s.claimAuthz(ctx, req)); err != nil {
		return nil, err // don't wrap, has status codes
	}

	claimant, err := uuid.Parse(req.ClaimantId)
	if err != nil {
		return nil, codeErrorf(codes.InvalidArgument, err, "failed to parse claimant ID")
	}
	duration := time.Duration(req.DurationMs) * time.Millisecond
	task, err := s.impl.TryClaim(ctx,
		entroq.From(req.Queues...),
		entroq.ClaimFor(duration),
		entroq.ClaimAs(claimant))
	if err != nil {
		return nil, wrapErrorf(err, "try claim")
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
	if err := s.Authorize(ctx, s.modifyAuthz(ctx, req)); err != nil {
		return nil, err // don't wrap, has status codes
	}

	claimant, err := uuid.Parse(req.ClaimantId)
	if err != nil {
		return nil, codeErrorf(codes.InvalidArgument, err, "failed to parse claimant ID")
	}
	modArgs := []entroq.ModifyArg{
		entroq.ModifyAs(claimant),
	}
	for _, insert := range req.Inserts {
		var (
			id  uuid.UUID
			err error
		)
		if insert.Id != "" {
			if id, err = uuid.Parse(insert.Id); err != nil {
				return nil, codeErrorf(codes.InvalidArgument, err, "failed to parse explicit insertion ID")
			}
		}
		modArgs = append(modArgs,
			entroq.InsertingInto(insert.Queue,
				entroq.WithArrivalTime(fromMS(insert.AtMs)),
				entroq.WithValue(insert.Value),
				entroq.WithID(id)))
	}
	for _, change := range req.Changes {
		id, err := uuid.Parse(change.GetOldId().Id)
		if err != nil {
			return nil, codeErrorf(codes.InvalidArgument, err, "failed to parse change id")
		}
		t := &entroq.Task{
			ID:        id,
			Version:   change.GetOldId().Version,
			FromQueue: change.GetOldId().Queue,
			Claimant:  claimant,
			Queue:     change.GetNewData().Queue,
			Value:     change.GetNewData().Value,
			At:        fromMS(change.GetNewData().AtMs),
		}
		modArgs = append(modArgs, entroq.Changing(t))
	}
	for _, del := range req.Deletes {
		id, err := uuid.Parse(del.Id)
		if err != nil {
			return nil, codeErrorf(codes.InvalidArgument, err, "failed to parse deletion id")
		}
		modArgs = append(modArgs, entroq.Deleting(id, del.Version, entroq.WithIDQueue(del.Queue)))
	}
	for _, dep := range req.Depends {
		id, err := uuid.Parse(dep.Id)
		if err != nil {
			return nil, codeErrorf(codes.InvalidArgument, err, "failed to parse depency id")
		}
		modArgs = append(modArgs, entroq.DependingOn(id, dep.Version, entroq.WithIDQueue(dep.Queue)))
	}
	inserted, changed, err := s.impl.Modify(ctx, modArgs...)
	if err != nil {
		if depErr, ok := entroq.AsDependency(err); ok {
			tmap := map[pb.ActionType][]*entroq.TaskID{
				pb.ActionType_INSERT: depErr.Inserts,
				pb.ActionType_DEPEND: depErr.Depends,
				pb.ActionType_DELETE: depErr.Deletes,
				pb.ActionType_CHANGE: depErr.Changes,
				pb.ActionType_CLAIM:  depErr.Claims,
			}

			details := []proto.Message{&pb.ModifyDep{
				Type: pb.ActionType_DETAIL,
				Msg:  depErr.Message,
			}}
			for dtype, dvals := range tmap {
				for _, tid := range dvals {
					details = append(details, &pb.ModifyDep{
						Type: dtype,
						Id:   &pb.TaskID{Id: tid.ID.String(), Version: tid.Version, Queue: tid.Queue},
					})
				}
			}

			stat, sErr := status.New(codes.NotFound, "modification dependency error").WithDetails(details...)
			if sErr != nil {
				return nil, codeErrorf(codes.NotFound, sErr, "dependency failed, and failed to add details %v", err)
			}
			return nil, stat.Err()
		}
		return nil, wrapErrorf(err, "modification failed")
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
	if err := s.Authorize(ctx, s.tasksAuthz(ctx, req)); err != nil {
		return nil, err // don't wrap, has status codes
	}

	claimant := uuid.Nil
	if req.ClaimantId != "" {
		var err error
		if claimant, err = uuid.Parse(req.ClaimantId); err != nil {
			return nil, codeErrorf(codes.InvalidArgument, err, "failed to parse claimant ID")
		}
	}
	// Claimant will only really be limited if it is nonzero.
	// Tasks will only be limited if non-empty.
	var ids []uuid.UUID
	for _, sID := range req.TaskId {
		id, err := uuid.Parse(sID)
		if err != nil {
			return nil, codeErrorf(codes.InvalidArgument, err, "invalid task ID: %q", sID)
		}
		ids = append(ids, id)
	}
	opts := []entroq.TasksOpt{
		entroq.LimitClaimant(claimant),
		entroq.WithTaskID(ids...),
		entroq.LimitTasks(int(req.Limit)),
	}
	if req.OmitValues {
		opts = append(opts, entroq.OmitValues())
	}
	tasks, err := s.impl.Tasks(ctx, req.Queue, opts...)
	if err != nil {
		return nil, wrapErrorf(err, "failed to get tasks")
	}
	resp := new(pb.TasksResponse)
	for _, task := range tasks {
		resp.Tasks = append(resp.Tasks, protoFromTask(task))
	}
	return resp, nil
}

func (s *QSvc) StreamTasks(req *pb.TasksRequest, stream pb.EntroQ_StreamTasksServer) error {
	resp, err := s.Tasks(stream.Context(), req)
	if err != nil {
		return wrapErrorf(err, "get tasks to stream")
	}

	// Note, we send a full TasksResponse each time because there might be
	// additional metadata added to that response later. This is more
	// future-proof.
	for _, task := range resp.Tasks {
		if err := stream.Send(&pb.TasksResponse{Tasks: []*pb.Task{task}}); err != nil {
			return wrapErrorf(err, "send stream tasks")
		}
	}
	return nil
}

// Queues returns a mapping from queue names to queue sizes.
func (s *QSvc) Queues(ctx context.Context, req *pb.QueuesRequest) (*pb.QueuesResponse, error) {
	queueMap, err := s.impl.Queues(ctx,
		entroq.MatchPrefix(req.MatchPrefix...),
		entroq.MatchExact(req.MatchExact...),
		entroq.LimitQueues(int(req.Limit)))
	if err != nil {
		return nil, wrapErrorf(err, "failed to get queues")
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
func (s *QSvc) QueueStats(ctx context.Context, req *pb.QueuesRequest) (*pb.QueuesResponse, error) {
	queueMap, err := s.impl.QueueStats(ctx,
		entroq.MatchPrefix(req.MatchPrefix...),
		entroq.MatchExact(req.MatchExact...),
		entroq.LimitQueues(int(req.Limit)))
	if err != nil {
		return nil, wrapErrorf(err, "failed to get queues")
	}
	resp := new(pb.QueuesResponse)
	for _, stat := range queueMap {
		resp.Queues = append(resp.Queues, &pb.QueueStats{
			Name:         stat.Name,
			NumTasks:     int32(stat.Size),
			NumClaimed:   int32(stat.Claimed),
			NumAvailable: int32(stat.Available),
			MaxClaims:    int32(stat.MaxClaims),
		})
	}
	return resp, nil
}

// Time returns the current time in milliseconds since the Epoch.
func (s *QSvc) Time(ctx context.Context, req *pb.TimeRequest) (*pb.TimeResponse, error) {
	return &pb.TimeResponse{TimeMs: toMS(time.Now().UTC())}, nil
}
