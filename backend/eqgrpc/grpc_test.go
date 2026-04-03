package eqgrpc_test

import (
	"context"
	"testing"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/backend/eqmem"
	"github.com/shiblon/entroq/qsvc/qtest"
)

func RunQTest(t *testing.T, tester qtest.Tester) {
	t.Helper()
	ctx := context.Background()
	client, stop, err := qtest.ClientService(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer client.Close()
	defer stop()
	tester(ctx, t, client, "grpctest/"+entroq.UUIDGenerator())
}

func TestGRPCTasksWithID(t *testing.T) {
	RunQTest(t, qtest.TasksWithID)
}

func TestGRPCTasksOmitValue(t *testing.T) {
	RunQTest(t, qtest.TasksOmitValue)
}

func TestGRPCTasksWithIDOnly(t *testing.T) {
	RunQTest(t, qtest.TasksWithIDOnly)
}

func TestGRPCInsertWithID(t *testing.T) {
	RunQTest(t, qtest.InsertWithID)
}

func TestGRPCSimpleSequence(t *testing.T) {
	RunQTest(t, qtest.SimpleSequence)
}

func TestGRPCSimpleChange(t *testing.T) {
	RunQTest(t, qtest.SimpleChange)
}

func TestGRPCSimpleWorker(t *testing.T) {
	RunQTest(t, qtest.SimpleWorker)
}

func TestGRPCMultiWorker(t *testing.T) {
	RunQTest(t, qtest.MultiWorker)
}

func TestGRPCWorkerMoveOnError(t *testing.T) {
	RunQTest(t, qtest.WorkerMoveOnError)
}

func TestGRPCWorkerRetryOnError(t *testing.T) {
	RunQTest(t, qtest.WorkerRetryOnError)
}

func TestGRPCWorkerRenewal(t *testing.T) {
	RunQTest(t, qtest.WorkerRenewal)
}

func TestGRPCClaimUnblocksOnNotify(t *testing.T) {
	RunQTest(t, qtest.ClaimUnblocksOnNotify)
}

func TestGRPCQueueMatch(t *testing.T) {
	RunQTest(t, qtest.QueueMatch)
}

func TestGRPCQueueStats(t *testing.T) {
	RunQTest(t, qtest.QueueStats)
}

func TestGRPCDeleteMissingTask(t *testing.T) {
	RunQTest(t, qtest.DeleteMissingTask)
}
