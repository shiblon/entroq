package eqgrpc_test

import (
	"context"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
)

// TestGRPCBlockingClaimUnblocksOnInsert verifies that a blocking Claim over gRPC
// holds (no per-attempt deadline, no spurious error) while no task is available
// and unblocks cleanly once one is inserted. This is the caller-visible contract
// of the single blocking Claim RPC that replaced the old client retry loop.
func TestGRPCBlockingClaimUnblocksOnInsert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stop, dial, err := eqtest.StartService(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("start service: %v", err)
	}
	defer stop()

	client, err := entroq.New(ctx, eqgrpc.Opener("bufnet",
		eqgrpc.WithNiladicDialer(dial),
		eqgrpc.WithInsecure(),
	))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	const q = "test/blocking-claim"

	claimDone := make(chan error, 1)
	go func() {
		_, err := client.Claim(ctx, entroq.From(q))
		claimDone <- err
	}()

	// Give the blocking claim time to be waiting on the empty queue.
	time.Sleep(300 * time.Millisecond)

	select {
	case err := <-claimDone:
		t.Fatalf("Claim returned before task was inserted: %v", err)
	default:
	}

	// Now insert a task -- Claim should unblock.
	if _, err := client.Modify(ctx, entroq.InsertingInto(q, entroq.WithValue("hi"))); err != nil {
		t.Fatalf("insert: %v", err)
	}

	select {
	case err := <-claimDone:
		if err != nil {
			t.Errorf("Claim returned error after task inserted: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for Claim to return after insert")
	}
}

func RunQTest(t *testing.T, tester eqtest.Tester) {
	t.Helper()
	ctx := context.Background()
	client, stop, err := eqtest.ClientService(ctx, eqmem.Opener())
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	// Use t.Cleanup so the server outlives any parallel subtests spawned by
	// tester. defer would fire when this function returns, before parallel
	// subtests finish, dropping the gRPC connection mid-flight.
	t.Cleanup(func() {
		client.Close()
		stop()
	})
	tester(ctx, t, client, "grpctest/"+entroq.GenHex16())
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

func TestGRPCTaskChangeFutureArrival(t *testing.T) {
	RunQTest(t, eqtest.TaskChangeFutureArrival)
}

func TestGRPCTaskChangeFarPastArrivalNormalized(t *testing.T) {
	RunQTest(t, eqtest.TaskChangeFarPastArrivalNormalized)
}

func TestGRPCModifyRejectsWrongQueue(t *testing.T) {
	RunQTest(t, eqtest.ModifyRejectsWrongQueue)
}

func TestGRPCEmptyWriteTargetRejected(t *testing.T) {
	RunQTest(t, eqtest.EmptyWriteTargetRejected)
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

func TestGRPCSimpleDocLifecycle(t *testing.T) {
	RunQTest(t, eqtest.SimpleDocLifecycle)
}

func TestGRPCDocMultiOp(t *testing.T) {
	RunQTest(t, eqtest.DocMultiOp)
}

func TestGRPCDocListing(t *testing.T) {
	RunQTest(t, eqtest.DocListing)
}

func TestGRPCDocConcurrencyStress(t *testing.T) {
	RunQTest(t, eqtest.DocConcurrencyStress)
}

func TestGRPCMixedAtomicStress(t *testing.T) {
	RunQTest(t, eqtest.MixedAtomicStress)
}

func TestGRPCDocClaimLocking(t *testing.T) {
	RunQTest(t, eqtest.DocClaimLocking)
}

func TestGRPCDocInsertWithID(t *testing.T) {
	RunQTest(t, eqtest.DocInsertWithID)
}

func TestGRPCDocClaimantBehavior(t *testing.T) {
	RunQTest(t, eqtest.DocClaimantBehavior)
}

func TestGRPCQueueStatsAccuracy(t *testing.T) {
	RunQTest(t, eqtest.QueueStatsAccuracy)
}

func TestGRPCNamespaceStats(t *testing.T) {
	RunQTest(t, eqtest.NamespaceStats)
}

func TestGRPCModifyReportsAllFailureClasses(t *testing.T) {
	RunQTest(t, eqtest.ModifyReportsAllFailureClasses)
}

func TestGRPCModifyRejectsWrongNamespace(t *testing.T) {
	RunQTest(t, eqtest.ModifyRejectsWrongNamespace)
}
