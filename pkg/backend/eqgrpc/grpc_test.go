package eqgrpc_test

import (
	"context"
	"testing"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
)

func RunQTest(t *testing.T, tester eqtest.Tester) {
	t.Helper()
	ctx := context.Background()
	client, stop, err := eqtest.ClientService(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer client.Close()
	defer stop()
	tester(ctx, t, client, "grpctest/"+entroq.Hex16Generator())
}

func TestGRPCTasksWithID(t *testing.T) {
	RunQTest(t, eqtest.TasksWithID)
}

func TestGRPCTasksOmitValue(t *testing.T) {
	RunQTest(t, eqtest.TasksOmitValue)
}

func TestGRPCTasksWithIDOnly(t *testing.T) {
	RunQTest(t, eqtest.TasksWithIDOnly)
}

func TestGRPCInsertWithID(t *testing.T) {
	RunQTest(t, eqtest.InsertWithID)
}

func TestGRPCSimpleSequence(t *testing.T) {
	RunQTest(t, eqtest.SimpleSequence)
}

func TestGRPCSimpleChange(t *testing.T) {
	RunQTest(t, eqtest.SimpleChange)
}

func TestGRPCSimpleWorker(t *testing.T) {
	RunQTest(t, eqtest.SimpleWorker)
}

func TestGRPCMultiWorker(t *testing.T) {
	RunQTest(t, eqtest.MultiWorker)
}

func TestGRPCWorkerMoveOnError(t *testing.T) {
	RunQTest(t, eqtest.WorkerMoveOnError)
}

func TestGRPCWorkerRetryOnError(t *testing.T) {
	RunQTest(t, eqtest.WorkerRetryOnError)
}

func TestGRPCWorkerRenewal(t *testing.T) {
	RunQTest(t, eqtest.WorkerRenewal)
}

func TestGRPCClaimUnblocksOnNotify(t *testing.T) {
	RunQTest(t, eqtest.ClaimUnblocksOnNotify)
}

func TestGRPCQueueMatch(t *testing.T) {
	RunQTest(t, eqtest.QueueMatch)
}

func TestGRPCQueueStats(t *testing.T) {
	RunQTest(t, eqtest.QueueStats)
}

func TestGRPCDeleteMissingTask(t *testing.T) {
	RunQTest(t, eqtest.DeleteMissingTask)
}

func TestGRPCWorkerDependencyHandler(t *testing.T) {
	RunQTest(t, eqtest.WorkerDependencyHandler)
}

func TestGRPCWorkerCompactDependencyHandler(t *testing.T) {
	RunQTest(t, eqtest.WorkerCompactDependencyHandler)
}

func TestGRPCWorkerRenewalNoDependencyHandler(t *testing.T) {
	RunQTest(t, eqtest.WorkerRenewalNoDependencyHandler)
}
