// Package rest provides a basic RESTful JSON backend for EntroQ.
// This can be used with the http mode of the EntroQ task service, set up thus:
//
// 	Server:
// 		qsvc (rest port) -> entroq library -> some backend (e.g., pg)
//
// 	Client:
// 		entroq library -> rest backend
//
// You can start, for example, a postgres-backed QSvc like this:
//
// 	ctx := context.Background()
// 	svc, err := qsvc.New(ctx, pg.Opener(dbHostPort)) // Other options available, too.
// 	if err != nil {
// 		log.Fatalf("Can't open PG backend: %v",e rr)
// 	}
// 	defer svc.Close()
//
// 	qsvc.HTTPListenAndServe(":37707", svc, "/api/v1")
//
// With the server set up this way, the client simply uses the EntroQ library,
// hands it the grpc Opener, and they're off:
//
// 	client, err := entroq.New(ctx, rest.Opener("myhost:37707"))
//
// That creates a client library that uses an HTTP/1.1 connection to do its work.
package rest // import "entrogo.com/entroq/rest"

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"entrogo.com/entroq"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "entrogo.com/entroq/proto"
)

const (
	DefaultAPIURL = "http://[::]:37707/api/v1"

	// ClaimRetryInterval is how long a rest client holds a claim request open
	// before dropping it and trying again.
	ClaimRetryInterval = 2 * time.Minute
)

// Opener creates an opener function to be used to talk to an HTTP REST JSON backend.
func Opener(apiURL string) entroq.BackendOpener {
	return func(ctx context.Context) (entroq.Backend, error) {
		return New(opts...)
	}
}

type backend struct {
	apiURL string
}

// New creates a new gRPC backend that attaches to the task service via gRPC.
// If empty, the API URL is assumed to be DefaultAPIURL.
func New(apiURL string) (*backend, error) {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	return &backend{
		apiURL: strings.TrimRight(apiURL, "/"),
	}, nil
}

// Close cleans up the REST client.
func (b *backend) Close() error {
	return nil
}

func (b *backend) urlFor(endpoint string) string {
	return b.apiURL + "/" + strings.TrimLeft(endpoint, "/")
}

func (b *backend) postProto(ctx context.Context, endpoint string, req proto.Message, emptyResp proto.Message) error {
	reqJSON, err := new(jsonpb.Marshaler).MarshalToString(req)
	if err != nil {
		return errors.Wrapf(err, "rest post %v marshal", endpoint)
	}

	// Use of context requires a slightly more complex make-request-then-do strategy.
	httpReq, err := http.NewRequestWithContext(ctx, "POST", b.urlFor(endpoint), strings.NewReader(reqJSON))
	if err != nil {
		return errors.Wrapf(err, "rest post new req", endpoint)
	}
	httpReq.Header.Set("Content-Tyep", "application/json")

	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		if err.(*url.Error).Timeout() {
			// Convert to something we recognize as a timeout more easily.
			return errors.Wrapf(context.DeadlineExceeded, "rest req timeout: %v", err)
		}
		return errors.Wrapf(err, "rest post %v", endpoint)
	}
	defer httpResp.Body.Close()

	switch httpResp.StatusCode {
	case http.StatusOK
		if err := jsonpb.Unmarshal(httpResp.Body, emptyResp); err != nil {
			return errors.Wrapf(err, "rest post %v", endpoint)
		}
		return nil
	case http.StatusFailedDependency:
		deps := new(pb.ModifyDeps)
		if err := jsonpb.Unmarshal(httpResp.Body, deps); err != nil {
			return errors.Wrapf(err, "rest post %v", endpoint)
		}

		depErr := entroq.DependencyError{}
		for _, detail := range deps.Deps {
			if detail.Type == pb.DepType_DETAIL {
				if detail.Msg != "" {
					depErr.Message += fmt.Sprintf(": %s", detail.Msg)
				}
				continue
			}

			// TODO: refactor - this is also in grpc (a few surrounding things are, as well).
			tid, err := fromTaskIDProto(detail.Id)
			if err != nil {
				return errors.Wrap(err, "rest dep detail from proto")
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
					return errors.Errorf("rest unknown dep type %v in detail %v", detail.Type, detail)
			}
		}
		return errors.Wrap(depErr, "rest modify dependency")
	case http.StatusRequestTimeout:
		return errors.Wrapf(context.DeadlineExceeded, "rest req timeout %q", httpResp.Status)
	default:
		body, err := ioutil.ReadAll(httpResp.Body)
		if err != nil {
			return errors.Error("rest response %q, can't read body: %v", httpResp.Status, err)
		}
		return errors.Errorf("rest response %q: %v", httpResp.Status, body)
	}
}

// Queues produces a mapping from queue names to queue sizes.
func (b *backend) Queues(ctx context.Context, qq *entroq.QueuesQuery) (map[string]int, error) {
	return entroq.QueuesFromStats(b.QueueStats(ctx, qq))
}

// QueueStats maps queue names to stats for those queues.
func (b *backend) QueueStats(ctx context.Context, qq *entroq.QueuesQuery) (map[string]*entroq.QueueStat, error) {
	resp := new(pb.QueuesResponse)
	if err := b.postProto(ctx, "queustats", &pb.QueuesRequest{
		MatchPrefix: qq.MatchPrefix,
		MatchExact:  qq.MatchExact,
		Limit:       int32(qq.Limit),
	}, resp); err != nil {
		return nil, errors.Wrap(err, "queuestats")
	}

	// TODO: refactor - this is in both grpc and rest, now.
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

	resp := new(pb.TasksResponse)
	if err := b.postProto(ctx, "tasks", &pb.TasksRequest{
		ClaimantId: tq.Claimant.String(),
		Queue:      tq.Queue,
		Limit:      int32(tq.Limit),
		TaskId:     ids,
		OmitValues: tq.OmitValues,
	}, resp); err != nil {
		return nil, errors.Wrap(err, "tasks")
	}

	// TODO: refactor - both in grpc and here now.
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
	// TODO: refactor - grpc has a lot of similar logic.
	for {
		// Check whether the parent context was canceled.
		select {
		case <-ctx.Done():
			return nil, errors.Wrap(ctx.Err(), "grpc claim")
		default:
		}
		ctx, _ := context.WithTimeout(ctx, ClaimRetryInterval)
		resp := new(pb.ClaimResponse)
		if err := b.postProto(ctx, "claim", &pb.ClaimRequest{
			ClaimantId: cq.Claimant.String(),
			Queues:     cq.Queues,
			DurationMs: int64(cq.Duration / time.Millisecond),
			PollMs:     int64(cq.PollTime / time.Millisecond),
		}, resp); err != nil {
			if entroq.IsTimeout(err) {
				// If we just timed out on our little request context, then
				// we can go around again.
				// It's possible that the *parent* context timed out, which
				// is why we check that at the beginning of the loop, as well.
				continue
			}
			return nil, errors.Wrap(err, "rest claim")
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
	resp := new(pb.ClaimResponse)
	if err := b.postProto(ctx, "tryclaim", &pb.ClaimRequest{
		ClaimantId: cq.Claimant.String(),
		Queues:     cq.Queues,
		DurationMs: int64(cq.Duration / time.Millisecond),
	}, resp); err != nil {
		return nil, errors.Wrap(err, "rest try claim")
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

	resp := new(pb.ModifyResponse)
	if err := b.postProto(ctx, req, resp); err != nil {
		// If we got a dependency error, that is already taken care of, so we can safely
		// pass all errors directly back.
		return nil, nil, errors.Wrap(err, "rest modify")
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
	resp := new(pb.TimeResponse)
	if err := b.postProto(ctx, new(pb.TimeRequest), resp); err != nil {
		return time.Time{}, errors.Wrap(err, "rest time")
	}
	return fromMS(resp.TimeMs).UTC(), nil
}
