// Package http provides an http REST backend for EntroQ.
package http

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"entrogo.com/entroq"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "entrogo.com/entroq/proto"
)

const (
	// DefaultAddr is the default listening address for http EntroQ services
	DefaultAddr = ":37707"

	// ClaimRetryInterval is how long an http client holds a claim request open
	// before dropping it and trying again.
	ClaimRetryInterval = 2 * time.Minute
)

// Opener creates an opener function that uses http to contact the EntroQ service.
func Opener(addr string) entroq.BackendOpener {
	if addr == "" {
		addr = DefaultAddr
	}

	return func(ctx context.Context) (entroq.Backend, error) {
		return &backend{addr}, nil
	}
}

type backend struct {
	addr string
}

// Close closes the underlying connection to the gRPC task service.
func (b *backend) Close() error {
	return nil
}

//////////////////

// Queues produces a mapping from queue names to queue sizes.
func (b *backend) Queues(ctx context.Context, qq *entroq.QueuesQuery) (map[string]int, error) {
	return entroq.QueuesFromStats(b.QueueStats(ctx, qq))
}

// QueueStats maps queue names to stats for those queues.
func (b *backend) QueueStats(ctx context.Context, qq *entroq.QueuesQuery) (map[string]*entroq.QueueStat, error) {

	qqProto := &pb.QueuesRequest{
		MatchPrefix: qq.MatchPrefix,
		MatchExact:  qq.MatchExact,
		Limit:       int32(qq.Limit),
	}

	jsonBytes, err := jsonpb.Marshal(qqProto)
	if err != nil {
		return nil, errors.Wrap(err, "marhsal proto to json")
	}

	rep, err := http.Post(b.addr, "queuesRequest", qq_json)
	if err != nil {
		return nil, errors.Wrap(err, "queuesRequest")
	}
	defer res.Body.Close()

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
		OmitValues: tq.OmitValues,
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
		Queues:     cq.Queues,
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

// Time returns the time as reported by the server.
func (b *backend) Time(ctx context.Context) (time.Time, error) {
	resp, err := pb.NewEntroQClient(b.conn).Time(ctx, new(pb.TimeRequest))
	if err != nil {
		return time.Time{}, errors.Wrap(err, "grpc time")
	}
	return fromMS(resp.TimeMs).UTC(), nil
}
