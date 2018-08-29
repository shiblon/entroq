package mem

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/google/uuid"
	"github.com/shiblon/entroq/qsvc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/shiblon/entroq/qsvc/proto"
)

func TestTryClaim(t *testing.T) {
	ctx := context.Background()

	conn := mustConn(ctx)
	defer conn.Close()

	client := pb.NewEntroQClient(conn)
	myID := uuid.New()
	nowMS := time.Now().UnixNano() / int64(time.Millisecond)

	sleep := func(d time.Duration) {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			log.Fatalf("Context canceled")
		}
	}

	const queue = "/test/TryClaim"

	// Claim from empty queue.
	claimResp, err := client.TryClaim(ctx, &pb.ClaimRequest{
		ClaimantId: myID.String(),
		Queue:      queue,
		DurationMs: 100,
	})
	if err != nil {
		t.Fatalf("Got unexpected error for claiming from an empty queue: %v", err)
	}
	if claimResp.Task != nil {
		t.Fatalf("Got unexpected non-nil claim response from empty queue:\n%v", proto.MarshalTextString(claimResp))
	}

	dataFromTask := func(task *pb.Task) *pb.TaskData {
		td := &pb.TaskData{
			Queue: task.Queue,
			AtMs:  task.AtMs,
			Value: make([]byte, len(task.Value)),
		}
		copy(td.Value, task.Value)
		return td
	}

	insTask1 := &pb.Task{
		Queue:      queue,
		AtMs:       nowMS,
		Value:      []byte("hello"),
		ClaimantId: myID.String(),
	}

	insTask2 := &pb.Task{
		Queue:      queue,
		AtMs:       nowMS + 100,
		Value:      []byte("there"),
		ClaimantId: myID.String(),
	}

	// Insert two tasks.
	modWant := &pb.ModifyResponse{Inserted: []*pb.Task{insTask1, insTask2}}
	modGot, err := client.Modify(ctx, &pb.ModifyRequest{
		ClaimantId: myID.String(),
		Inserts:    []*pb.TaskData{dataFromTask(insTask1), dataFromTask(insTask2)},
	})
	if err != nil {
		t.Fatalf("Got unexpected error inserting two tasks: %v", err)
	}
	if err := equalModify(modWant, modGot, 0); err != nil {
		t.Fatal(err)
	}
	// Also check that their arrival times are 100 ms apart as expected:
	if at0, at1 := modGot.Inserted[0].AtMs, modGot.Inserted[1].AtMs; at1-at0 != 100 {
		t.Fatalf("Wanted At difference to be %d, got %d: protos\nWant\n%v\nGot\n%v",
			100, at1-at0, proto.MarshalTextString(modWant), proto.MarshalTextString(modGot))
	}

	// Claim ready task.
	claimResp, err = client.TryClaim(ctx, &pb.ClaimRequest{
		ClaimantId: myID.String(),
		Queue:      queue,
		DurationMs: 10000,
	})
	if err != nil {
		t.Fatalf("Got unexpected error for claiming from a queue with one ready task: %v", err)
	}
	if err := equalTasks(insTask1, claimResp.Task, 1); err != nil {
		t.Fatalf("Tasks were not as expected when claiming from a queue with one ready task: %v", err)
	}
	// Also check that the arrival time is well into the future.
	if got, lowerBound := claimResp.Task.AtMs, nowMS+10000; got < lowerBound {
		t.Fatalf("Claiming a task, expected arrival time of at least %d, got %d", lowerBound, got)
	}
	if got, upperBound := claimResp.Task.AtMs, nowMS+12500; got > upperBound {
		t.Fatalf("Expected arrival time at most %d, got %d", upperBound, got)
	}
	if claimResp.Task == nil {
		t.Fatalf("Got unexpected nil claim response from queue with one ready task:\n%v", proto.MarshalTextString(claimResp))
	}

	// Claim not ready task.
	claimResp, err = client.TryClaim(ctx, &pb.ClaimRequest{
		ClaimantId: myID.String(),
		Queue:      queue,
		DurationMs: 10000,
	})
	if err != nil {
		t.Fatalf("Got unexpected error for claiming from a queue with no ready tasks: %v", err)
	}
	if claimResp.Task != nil {
		t.Fatalf("Got unexpected non-nil claim response from a queue with no ready tasks:\n%v", proto.MarshalTextString(claimResp))
	}

	// Make sure the next claim will work.
	sleep(100 * time.Millisecond)
	claimResp, err = client.TryClaim(ctx, &pb.ClaimRequest{
		ClaimantId: myID.String(),
		Queue:      queue,
		DurationMs: 5000,
	})
	if err != nil {
		t.Fatalf("Got unexpected error for claiming from a queue with no ready tasks: %v", err)
	}
	if err := equalTasks(insTask2, claimResp.Task, 1); err != nil {
		t.Fatalf("Tasks were not as expected when claiming second task from queue: %v", err)
	}
	if got, lowerBound := claimResp.Task.AtMs, nowMS+5000; got < lowerBound {
		t.Fatalf("Expected arrival time at least %d, got %d", lowerBound, got)
	}
	if got, upperBound := claimResp.Task.AtMs, nowMS+7500; got > upperBound {
		t.Fatalf("Expected arrival time at most %d, got %d", upperBound, got)
	}
}

var lis *bufconn.Listener

const bufSize = 1 << 20

func init() {
	lis = bufconn.Listen(bufSize)

	svc, err := qsvc.New(context.Background(), Opener())
	if err != nil {
		log.Fatalf("Failed to open qsvc with mem backend: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterEntroQServer(s, svc)
	go s.Serve(lis)
}

func bufDialer(string, time.Duration) (net.Conn, error) {
	return lis.Dial()
}

func mustConn(ctx context.Context) *grpc.ClientConn {
	conn, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithDialer(bufDialer),
		grpc.WithInsecure())
	if err != nil {
		log.Fatalf("Could not dial: %v", err)
	}
	return conn
}

func equalProto(want, got proto.Message) error {
	if !proto.Equal(want, got) {
		return fmt.Errorf("Wanted:\n%vGot:\n%v", proto.MarshalTextString(want), proto.MarshalTextString(got))
	}
	return nil
}

func equalTasks(want, got proto.Message, versionChange int) error {
	ta, tb := want.(*pb.Task), got.(*pb.Task)
	if (ta == nil) != (tb == nil) {
		return fmt.Errorf("Wanted equal tasks, but one is nil and one is not: want\n%v\ngot\n%v", proto.MarshalTextString(want), proto.MarshalTextString(got))
	}
	if ta == nil {
		return nil
	}
	if ta.Queue != tb.Queue ||
		ta.Version+int32(versionChange) != tb.Version ||
		ta.ClaimantId != tb.ClaimantId ||
		!bytes.Equal(ta.Value, tb.Value) {
		return fmt.Errorf("Wanted (ignoring id, at_ms, created_ms, and modified_ms):\n%vGot:\n%v",
			proto.MarshalTextString(want), proto.MarshalTextString(got))
	}
	return nil
}

func equalModify(want, got proto.Message, modVersionChange int) error {
	rWant, rGot := want.(*pb.ModifyResponse), got.(*pb.ModifyResponse)
	if w, g := len(rWant.Inserted), len(rGot.Inserted); w != g {
		return fmt.Errorf("Wanted %d inserted, got %d", w, g)
	}
	if w, g := len(rWant.Changed), len(rGot.Changed); w != g {
		return fmt.Errorf("Wanted %d changed, got %d", w, g)
	}
	for i, insWant := range rWant.Inserted {
		insGot := rGot.Inserted[i]
		if err := equalTasks(insWant, insGot, 0); err != nil {
			return fmt.Errorf("in insertion:\n%v", err)
		}
	}
	for i, chgWant := range rWant.Changed {
		chgGot := rGot.Changed[i]
		if err := equalTasks(chgWant, chgGot, modVersionChange); err != nil {
			return fmt.Errorf("in change:\n%v", err)
		}
	}
	return nil
}
