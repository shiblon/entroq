// Package qtest contains standard testing routines for exercising various backends in similar ways.
package qtest

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/qsvc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/shiblon/entroq/proto"
	hpb "google.golang.org/grpc/health/grpc_health_v1"
)

const bufSize = 1 << 20

// Dialer returns a net connection.
type Dialer func() (net.Conn, error)

// StartService starts an in-memory gRPC network service and returns a function for creating client connections to it.
func StartService(ctx context.Context, opener entroq.BackendOpener) (*grpc.Server, Dialer, error) {
	lis := bufconn.Listen(bufSize)
	svc, err := qsvc.New(ctx, opener)
	if err != nil {
		return nil, nil, fmt.Errorf("start service: %v", err)
	}
	s := grpc.NewServer()
	hpb.RegisterHealthServer(s, health.NewServer())
	pb.RegisterEntroQServer(s, svc)
	go s.Serve(lis)

	return s, lis.Dial, nil
}

// SimpleSequence tests some basic functionality of a task manager, over gRPC.
func SimpleSequence(ctx context.Context, t *testing.T, client *entroq.EntroQ) {
	t.Helper()

	now := time.Now()

	sleep := func(d time.Duration) {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			log.Fatalf("Context canceled")
		}
	}

	const queue = "/test/TryClaim"

	// Claim from empty queue.
	task, err := client.TryClaim(ctx, queue, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Got unexpected error for claiming from an empty queue: %v", err)
	}
	if task != nil {
		t.Fatalf("Got unexpected non-nil claim response from empty queue:\n%s", task)
	}

	insWant := []*entroq.Task{
		{
			Queue:    queue,
			At:       now,
			Value:    []byte("hello"),
			Claimant: client.ID(),
		},
		{
			Queue:    queue,
			At:       now.Add(100 * time.Millisecond),
			Value:    []byte("there"),
			Claimant: client.ID(),
		},
	}
	var insData []*entroq.TaskData
	for _, task := range insWant {
		insData = append(insData, task.Data())
	}

	inserted, changed, err := client.Modify(ctx, entroq.Inserting(insData...))
	if err != nil {
		t.Fatalf("Got unexpected error inserting two tasks: %v", err)
	}
	if changed != nil {
		t.Fatalf("Got unexpected changes during insertion: %v", err)
	}
	if diff := EqualAllTasks(insWant, inserted); diff != "" {
		t.Fatalf("Modify tasks unexpected result, ignoring ID and time fields (-want +got):\n%v", diff)
	}
	// Also check that their arrival times are 100 ms apart as expected:
	if diff := inserted[1].At.Sub(inserted[0].At); diff != 100*time.Millisecond {
		t.Fatalf("Wanted At difference to be %v, got %v", 100*time.Millisecond, diff)
	}

	// Get queues.
	queuesWant := map[string]int{queue: 2}
	queuesGot, err := client.Queues(ctx)
	if err != nil {
		t.Fatalf("Getting queues failed: %v", err)
	}
	if diff := cmp.Diff(queuesWant, queuesGot); diff != "" {
		t.Fatalf("Queues (-want +got):\n%v", diff)
	}

	// Get all tasks.
	tasksGot, err := client.Tasks(ctx, queue)
	if err != nil {
		t.Fatalf("Tasks call failed after insertions: %v", err)
	}
	if diff := EqualAllTasks(insWant, tasksGot); diff != "" {
		t.Fatalf("Tasks unexpected return, ignoring ID and time fields (-want +got):\n%v", diff)
	}

	// Claim ready task.
	claimCtx, _ := context.WithTimeout(ctx, 10*time.Millisecond)
	claimed, err := client.Claim(claimCtx, queue, 10*time.Second)

	if err != nil {
		t.Fatalf("Got unexpected error for claiming from a queue with one ready task: %v", err)
	}
	if claimed == nil {
		t.Fatalf("Unexpected nil result from blocking Claim")
	}
	if diff := EqualTasksVersionIncr(insWant[0], claimed, 1); diff != "" {
		t.Fatalf("Claim tasks differ, ignoring ID and times:\n%v", diff)
	}
	if got, lower, upper := claimed.At, now.Add(9*time.Second), now.Add(11*time.Second); got.Before(lower) || got.After(upper) {
		t.Fatalf("Claimed arrival time not in time bounds [%v, %v]: %v", lower, upper, claimed.At)
	}

	// TryClaim not ready task.
	tryclaimed, err := client.TryClaim(ctx, queue, 10*time.Second)
	if err != nil {
		t.Fatalf("Got unexpected error for claiming from a queue with no ready tasks: %v", err)
	}
	if tryclaimed != nil {
		t.Fatalf("Got unexpected non-nil claim response from a queue with no ready tasks:\n%s", tryclaimed)
	}

	// Make sure the next claim will work.
	sleep(100 * time.Millisecond)
	tryclaimed, err = client.TryClaim(ctx, queue, 5*time.Second)
	if err != nil {
		t.Fatalf("Got unexpected error for claiming from a queue with one ready task: %v", err)
	}
	if diff := EqualTasksVersionIncr(insWant[1], tryclaimed, 1); diff != "" {
		t.Fatalf("TryClaim got unexpected task, ignoring ID and time fields (-want +got):\n%v", diff)
	}
	if got, lower, upper := tryclaimed.At, time.Now().Add(4*time.Second), time.Now().Add(6*time.Second); got.Before(lower) || got.After(upper) {
		t.Fatalf("TryClaimed arrival time not in time bounds [%v, %v]: %v", lower, upper, tryclaimed.At)
	}
}

// EqualAllTasks returns a string diff if any of the tasks in the lists are unequal.
func EqualAllTasks(want, got []*entroq.Task) string {
	if len(want) != len(got) {
		return cmp.Diff(want, got)
	}
	if len(want) == 0 {
		return ""
	}
	var diffs []string
	for i, w := range want {
		g := got[i]
		if w.Queue != g.Queue || w.Claimant != g.Claimant || !bytes.Equal(w.Value, g.Value) {
			diffs = append(diffs, cmp.Diff(w, g))
		}
	}
	if len(diffs) != 0 {
		return strings.Join(diffs, "\n")
	}
	return ""
}

// EqualAllTasksVersionIncr returns a non-empty diff if any of the tasks are
// unequal, taking a version increment into account for the 'got' tasks.
func EqualAllTasksVersionIncr(want, got []*entroq.Task, versionBump int) string {
	if diff := EqualAllTasks(want, got); diff != "" {
		return diff
	}
	var diffs []string
	for i, w := range want {
		g := got[i]
		if w.Version+int32(versionBump) != g.Version {
			diffs = append(diffs, cmp.Diff(g, w))
		}
	}
	if len(diffs) != 0 {
		return strings.Join(diffs, "\n")
	}
	return ""
}

// Checks for task equality, ignoring ID, Version, and all time fields.
func EqualTasks(want, got *entroq.Task) string {
	return EqualAllTasks([]*entroq.Task{want}, []*entroq.Task{got})
}

// EqualTasksVersionIncr checks for equality, allowing a version increment.
func EqualTasksVersionIncr(want, got *entroq.Task, versionBump int) string {
	return EqualAllTasksVersionIncr([]*entroq.Task{want}, []*entroq.Task{got}, versionBump)
}
