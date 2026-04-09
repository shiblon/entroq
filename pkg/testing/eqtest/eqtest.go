// Package eqtest contains standard testing routines for exercising various backends in similar ways.
package eqtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/eqsvcgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/shiblon/entroq/api"
	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

const bufSize = 1 << 20

// Dialer returns a net connection.
type Dialer func() (net.Conn, error)

// Tester runs a test helper, all of which these test functions are.
type Tester func(ctx context.Context, t *testing.T, client *entroq.EntroQ, qPrefix string)

// ClientService starts an in-memory gRPC network service via StartService,
// then creates an EntroQ client that connects to it. It returns the client and
// a function that can be deferred for cleanup.
//
// The opener is used by the service to connect to storage. The client always
// uses a grpc opener.
func ClientService(ctx context.Context, opener entroq.BackendOpener) (client *entroq.EntroQ, stop func(), err error) {
	s, dial, err := StartService(ctx, opener)
	if err != nil {
		return nil, nil, fmt.Errorf("client service: %w", err)
	}
	defer func() {
		if err != nil {
			s.Stop()
		}
	}()

	client, err = entroq.New(ctx, eqgrpc.Opener("bufnet",
		eqgrpc.WithNiladicDialer(dial),
		eqgrpc.WithInsecure()))
	if err != nil {
		return nil, nil, fmt.Errorf("start client on in-memory service: %w", err)
	}

	return client, func() {
		client.Close()
		s.Stop()
	}, nil
}

// StartService starts an in-memory gRPC network service and returns a function for creating client connections to it.
func StartService(ctx context.Context, opener entroq.BackendOpener) (*grpc.Server, Dialer, error) {
	lis := bufconn.Listen(bufSize)
	svc, err := eqsvcgrpc.New(ctx, opener)
	if err != nil {
		return nil, nil, fmt.Errorf("start service: %w", err)
	}
	s := grpc.NewServer()
	hpb.RegisterHealthServer(s, health.NewServer())
	pb.RegisterEntroQServer(s, svc)
	go s.Serve(lis)

	return s, lis.Dial, nil
}

func newTaskQueueVersionValue(t *entroq.Task) *taskQueueVersionValue {
	return &taskQueueVersionValue{
		Queue:   t.Queue,
		Version: t.Version,
		Value:   append(json.RawMessage(nil), t.Value...),
	}
}

type taskIDQueueVersionValue struct {
	Queue    string
	ID       string
	Version  int32
	Value    json.RawMessage
	Claimant string
}

func newTaskIDQueueVersionValue(t *entroq.Task) *taskIDQueueVersionValue {
	return &taskIDQueueVersionValue{
		ID:       t.ID,
		Queue:    t.Queue,
		Version:  t.Version,
		Claimant: t.Claimant,
		Value:    append(json.RawMessage(nil), t.Value...),
	}
}

type cmpOpts struct {
	versionIncr int
	valueEmpty  bool
}

type cmpOpt func(*cmpOpts)

func expectVersionIncr(by int) cmpOpt {
	if by < 0 {
		by = 0
	}
	return func(o *cmpOpts) {
		o.versionIncr = by
	}
}

func expectEmptyValue() cmpOpt {
	return func(o *cmpOpts) {
		o.valueEmpty = true
	}
}

// EqualAllTasksUnorderedSkipTimesAndCounters returns a diff if any non-time
// fields are different between unordered task lists. Sorts by ID to accomplish
// this.
func EqualAllTasksUnorderedSkipTimesAndCounters(want, got []*entroq.Task, opts ...cmpOpt) string {
	options := new(cmpOpts)
	for _, o := range opts {
		o(options)
	}
	ws := make([]*taskIDQueueVersionValue, len(want))
	for i, t := range want {
		ws[i] = newTaskIDQueueVersionValue(t)
		if options.versionIncr != 0 {
			ws[i].Version += int32(options.versionIncr)
		}
		if options.valueEmpty {
			ws[i].Value = nil
		}
	}
	gs := make([]*taskIDQueueVersionValue, len(got))
	for i, t := range got {
		gs[i] = newTaskIDQueueVersionValue(t)
	}

	sort.Slice(ws, func(i, j int) bool {
		return ws[i].ID < ws[j].ID
	})

	sort.Slice(gs, func(i, j int) bool {
		return gs[i].ID < gs[j].ID
	})

	return cmp.Diff(ws, gs)
}

// EqualAllTasksUnorderedByValue checks that all task values from want are represented in got.
// It assumes IDs and queues are not important. Suitable for tests that set a
// unique value per task, but don't have ID information. Returns a string diff if different.
func EqualAllTasksUnorderedByValue(want, got []*entroq.Task) string {
	wantVals := make([]json.RawMessage, len(want))
	for i, w := range want {
		wantVals[i] = append(json.RawMessage(nil), w.Value...)
	}

	gotVals := make([]json.RawMessage, len(got))
	for i, w := range got {
		gotVals[i] = append(json.RawMessage(nil), w.Value...)
	}

	sort.Slice(wantVals, func(a, b int) bool {
		return bytes.Compare(wantVals[a], wantVals[b]) < 0
	})
	sort.Slice(gotVals, func(a, b int) bool {
		return bytes.Compare(gotVals[a], gotVals[b]) < 0
	})
	return cmp.Diff(wantVals, gotVals)
}

// EqualAllTasksOrderedSkipIDAndTime returns a string diff if the order of
// tasks or values of tasks in two slices is not equal. The slices should also be
// the same length.
func EqualAllTasksOrderedSkipIDAndTime(want, got []*entroq.Task) string {
	gotSimplified := make([]*taskQueueVersionValue, len(got))
	for i, t := range got {
		gotSimplified[i] = newTaskQueueVersionValue(t)
	}

	wantSimplified := make([]*taskQueueVersionValue, len(want))
	for i, t := range want {
		wantSimplified[i] = newTaskQueueVersionValue(t)
	}

	return cmp.Diff(wantSimplified, gotSimplified)
}

// EqualTasksVersionIncr checks for equality, allowing a version increment.
func EqualTasksVersionIncr(want, got *entroq.Task, versionBump int) string {
	return EqualAllTasksUnorderedSkipTimesAndCounters([]*entroq.Task{want}, []*entroq.Task{got}, expectVersionIncr(versionBump))
}

// DeleteMissingTask tests that deleting non-existent tasks or versions results
// in the right dependency errors.
