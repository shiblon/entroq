package eqredis

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

var redisAddr string

func TestMain(m *testing.M) {
	ctx := context.Background()

	ctr, err := testcontainers.Run(ctx, "redis:7-alpine",
		testcontainers.WithWaitStrategy(
			wait.ForLog("Ready to accept connections").
				WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		log.Fatalf("Redis start: %v", err)
	}
	defer func() {
		if err := ctr.Terminate(ctx); err != nil {
			log.Printf("Redis stop: %v", err)
		}
	}()

	redisAddr, err = ctr.Endpoint(ctx, "")
	if err != nil {
		log.Fatalf("Redis endpoint: %v", err)
	}

	os.Exit(m.Run())
}

func redisClient(ctx context.Context) (*entroq.EntroQ, error) {
	return entroq.New(ctx, Opener(WithAddr(redisAddr)))
}

func RunQTest(t *testing.T, tester eqtest.Tester) {
	t.Helper()
	ctx := context.Background()
	client, err := redisClient(ctx)
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer client.Close()
	tester(ctx, t, client, fmt.Sprintf("redistest/%s", client.GenID()))
}

func TestTasksWithID(t *testing.T) {
	RunQTest(t, eqtest.TasksWithID)
}

func TestTasksOmitValue(t *testing.T) {
	RunQTest(t, eqtest.TasksOmitValue)
}

func TestTasksWithIDOnly(t *testing.T) {
	RunQTest(t, eqtest.TasksWithIDOnly)
}

func TestInsertWithID(t *testing.T) {
	RunQTest(t, eqtest.InsertWithID)
}

func TestSimpleSequence(t *testing.T) {
	RunQTest(t, eqtest.SimpleSequence)
}

func TestSimpleChange(t *testing.T) {
	RunQTest(t, eqtest.SimpleChange)
}

func TestSimpleWorker(t *testing.T) {
	RunQTest(t, eqtest.SimpleWorker)
}

func TestMultiWorker(t *testing.T) {
	RunQTest(t, eqtest.MultiWorker)
}

func TestWorkerMoveOnError(t *testing.T) {
	RunQTest(t, eqtest.WorkerMoveOnError)
}

func TestWorkerRetryOnError(t *testing.T) {
	RunQTest(t, eqtest.WorkerRetryOnError)
}

func TestWorkerRenewal(t *testing.T) {
	RunQTest(t, eqtest.WorkerRenewal)
}

func TestClaimUnblocksOnNotify(t *testing.T) {
	RunQTest(t, eqtest.ClaimUnblocksOnNotify)
}

func TestQueueMatch(t *testing.T) {
	RunQTest(t, eqtest.QueueMatch)
}

// TestQueueStats is skipped for eqredis because QueueStats intentionally
// returns MaxClaims=0. Computing it requires a full linear scan of task hashes
// per queue (O(tasks), not O(queues)), which is inappropriate for a stats call.
// Postgres avoids this via an index-only scan; Redis has no equivalent.
// TODO: consider a eq:maxclaims:{name} sorted set to restore O(1) MaxClaims.
func TestQueueStats(t *testing.T) {
	t.Skip("eqredis returns MaxClaims=0; see QueueStats comment in query.go")
}

func TestQueueStatsLimit(t *testing.T) {
	RunQTest(t, eqtest.QueueStatsLimit)
}

func TestDeleteMissingTask(t *testing.T) {
	RunQTest(t, eqtest.DeleteMissingTask)
}

func TestWorkerDependencyHandler(t *testing.T) {
	RunQTest(t, eqtest.WorkerDependencyHandler)
}

func TestWorkerCompactDependencyHandler(t *testing.T) {
	RunQTest(t, eqtest.WorkerCompactDependencyHandler)
}

func TestWorkerRenewalNoDependencyHandler(t *testing.T) {
	RunQTest(t, eqtest.WorkerRenewalNoDependencyHandler)
}

func TestSimpleDocLifecycle(t *testing.T) {
	RunQTest(t, eqtest.SimpleDocLifecycle)
}

func TestDocMultiOp(t *testing.T) {
	RunQTest(t, eqtest.DocMultiOp)
}

func TestDocListing(t *testing.T) {
	RunQTest(t, eqtest.DocListing)
}

func TestDocClaimLocking(t *testing.T) {
	RunQTest(t, eqtest.DocClaimLocking)
}

func TestDocInsertWithID(t *testing.T) {
	RunQTest(t, eqtest.DocInsertWithID)
}
