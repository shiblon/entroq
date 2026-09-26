package eqredis

import (
	"context"
	"fmt"
	"log"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/internal/latency"
	"github.com/shiblon/entroq/pkg/testing/eqtest"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var redisAddr string

// dockerAvailable reports whether a Docker daemon is reachable. The eqredis
// tests run Redis in a testcontainer, so without Docker there is nothing to
// test against. Detecting its absence lets TestMain skip cleanly (exit 0)
// instead of hard-failing the package on a bare `go test ./...` or a CI without
// a Docker service.
func dockerAvailable(ctx context.Context) bool {
	cli, err := testcontainers.NewDockerClient()
	if err != nil {
		return false
	}
	defer cli.Close()
	// NewDockerClient does not fail when the daemon is unreachable (it swallows
	// the probe and hands back an env-derived client), so the ping is the real
	// check. Bound it: an unreachable or black-hole endpoint must not hang here.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = cli.Ping(ctx)
	return err == nil
}

func TestMain(m *testing.M) {
	ctx := context.Background()

	if !dockerAvailable(ctx) {
		log.Println("SKIP: Docker is not available; skipping eqredis integration tests (they require a Redis testcontainer).")
		os.Exit(0)
	}

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

func TestReadinessFanout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const interval = 500 * time.Millisecond
	client, err := entroq.New(ctx, Opener(WithAddr(redisAddr), WithReadinessInterval(interval)))
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer client.Close()
	eqtest.ReadinessFanout(interval)(ctx, t, client, fmt.Sprintf("redistest/%s", client.GenID()))
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

func TestChangeKeepsStoredFields(t *testing.T) {
	RunQTest(t, eqtest.ChangeKeepsStoredFields)
}

func TestInsertKeepsAttemptAndErr(t *testing.T) {
	RunQTest(t, eqtest.InsertKeepsAttemptAndErr)
}

func TestModifyRejectsDuplicateIDs(t *testing.T) {
	RunQTest(t, eqtest.ModifyRejectsDuplicateIDs)
}

func TestModifyRespectsTaskClaims(t *testing.T) {
	RunQTest(t, eqtest.ModifyRespectsTaskClaims)
}

func TestTasksWithIDStaysInQueue(t *testing.T) {
	RunQTest(t, eqtest.TasksWithIDStaysInQueue)
}

func TestDocGroups(t *testing.T) {
	RunQTest(t, eqtest.DocGroups)
}

func TestTaskChangeFarPastArrivalNormalized(t *testing.T) {
	RunQTest(t, eqtest.TaskChangeFarPastArrivalNormalized)
}

func TestModifyRejectsWrongQueue(t *testing.T) {
	RunQTest(t, eqtest.ModifyRejectsWrongQueue)
}

func TestEmptyWriteTargetRejected(t *testing.T) {
	RunQTest(t, eqtest.EmptyWriteTargetRejected)
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

func TestClaimUnblocksOnNotify(t *testing.T) {
	RunQTest(t, eqtest.ClaimUnblocksOnNotify)
}

func TestQueueMatch(t *testing.T) {
	RunQTest(t, eqtest.QueueMatch)
}

func TestQueuePrefixMatchLiteral(t *testing.T) {
	RunQTest(t, eqtest.QueuePrefixMatchLiteral)
}

func TestNamespacePrefixMatchLiteral(t *testing.T) {
	RunQTest(t, eqtest.NamespacePrefixMatchLiteral)
}

func TestQueueStats(t *testing.T) {
	RunQTest(t, eqtest.QueueStats)
}

func TestQueueStatsLimit(t *testing.T) {
	RunQTest(t, eqtest.QueueStatsLimit)
}

func TestDeleteMissingTask(t *testing.T) {
	RunQTest(t, eqtest.DeleteMissingTask)
}

func TestClaimRandomHead(t *testing.T) {
	RunQTest(t, eqtest.ClaimRandomHead)
}

func TestTasksClaimantLimit(t *testing.T) {
	RunQTest(t, eqtest.TasksClaimantLimit)
}

func TestLengthLimits(t *testing.T) {
	RunQTest(t, eqtest.LengthLimits)
}

func TestClaimLongDuration(t *testing.T) {
	RunQTest(t, eqtest.ClaimLongDuration)
}

func TestMapReduce(t *testing.T) {
	RunQTest(t, eqtest.MapReduce)
}

func TestWorkerCompactDependencyHandler(t *testing.T) {
	RunQTest(t, eqtest.WorkerCompactDependencyHandler)
}

func TestWorkerDependencyMove(t *testing.T) {
	RunQTest(t, eqtest.WorkerDependencyMove)
}

func TestWorkerHoldsEmptyGroup(t *testing.T) {
	RunQTest(t, eqtest.WorkerHoldsEmptyGroup)
}

func TestSimpleDocLifecycle(t *testing.T) {
	RunQTest(t, eqtest.SimpleDocLifecycle)
}

func TestInitialVersions(t *testing.T) {
	RunQTest(t, eqtest.InitialVersions)
}

func TestDocMultiOp(t *testing.T) {
	RunQTest(t, eqtest.DocMultiOp)
}

func TestDocTimestamps(t *testing.T) {
	RunQTest(t, eqtest.DocTimestamps)
}

func TestDocListing(t *testing.T) {
	RunQTest(t, eqtest.DocListing)
}

func TestDocKeyRangeByteOrder(t *testing.T) {
	RunQTest(t, eqtest.DocKeyRangeByteOrder)
}

func TestDocClaimLocking(t *testing.T) {
	RunQTest(t, eqtest.DocClaimLocking)
}

func TestDocInsertWithID(t *testing.T) {
	RunQTest(t, eqtest.DocInsertWithID)
}

func TestDocClaimantBehavior(t *testing.T) {
	RunQTest(t, eqtest.DocClaimantBehavior)
}

func TestQueueStatsAccuracy(t *testing.T) {
	RunQTest(t, eqtest.QueueStatsAccuracy)
}

func TestNamespaceStats(t *testing.T) {
	RunQTest(t, eqtest.NamespaceStats)
}

func TestModifyReportsAllFailureClasses(t *testing.T) {
	RunQTest(t, eqtest.ModifyReportsAllFailureClasses)
}

func TestModifyRejectsWrongNamespace(t *testing.T) {
	RunQTest(t, eqtest.ModifyRejectsWrongNamespace)
}

func TestInvalidRequests(t *testing.T) {
	RunQTest(t, eqtest.InvalidRequests)
}

func TestBackendRejectsInvalidRequests(t *testing.T) {
	ctx := context.Background()
	b, err := Open(ctx, WithAddr(redisAddr))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer b.Close()
	eqtest.BackendRejectsInvalidRequests(ctx, t, b, "/redistest/"+entroq.GenHex16())
	eqtest.StorageRejectsZeroDurations(ctx, t, b, "/redistest/"+entroq.GenHex16())
}

func TestTasksClaimantFilter(t *testing.T) {
	RunQTest(t, eqtest.TasksClaimantFilter)
}

func TestQueueStatsCounts(t *testing.T) {
	RunQTest(t, eqtest.QueueStatsCounts)
}

func TestQueueStatsMatching(t *testing.T) {
	RunQTest(t, eqtest.QueueStatsMatching)
}

func TestDocsOrderAndLimits(t *testing.T) {
	RunQTest(t, eqtest.DocsOrderAndLimits)
}

func TestTaskClaimantIsHolder(t *testing.T) {
	RunQTest(t, eqtest.TaskClaimantIsHolder)
}

func TestTaskChangeFutureArrival(t *testing.T) {
	RunQTest(t, eqtest.TaskChangeFutureArrival)
}

// TestDocConcurrencyStress is slow, so it runs in parallel with the other parallel tests
// once the sequential ones finish; it uses its own namespace and queue.
func TestDocConcurrencyStress(t *testing.T) {
	t.Parallel()
	RunQTest(t, eqtest.DocConcurrencyStress)
}

// TestMixedAtomicStress is slow, so it runs in parallel with the other parallel tests
// once the sequential ones finish; it uses its own namespace and queue.
func TestMixedAtomicStress(t *testing.T) {
	t.Parallel()
	RunQTest(t, eqtest.MixedAtomicStress)
}

// TestLatencyMetrics checks that Redis records claim and modify durations, as
// the other backends do, with the shared latency buckets.
func TestLatencyMetrics(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	b, err := Open(ctx, WithAddr(redisAddr), WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer b.Close()
	client, err := entroq.New(ctx, func(context.Context) (entroq.Backend, error) { return b, nil })
	if err != nil {
		t.Fatal(err)
	}
	queue := "latency-metrics/" + entroq.GenHex16()
	if _, err := client.Modify(ctx, entroq.InsertingInto(queue)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.TryClaim(ctx, entroq.From(queue)); err != nil {
		t.Fatal(err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok || (m.Name != "entroq.claim.duration" && m.Name != "entroq.modify.duration") {
				continue
			}
			seen[m.Name] = true
			if dp := h.DataPoints[0]; dp.Count == 0 || !slices.Equal(dp.Bounds, latency.Bounds) {
				t.Errorf("%s: count %d, bounds %v; want recorded with %v", m.Name, dp.Count, dp.Bounds, latency.Bounds)
			}
		}
	}
	for _, name := range []string{"entroq.claim.duration", "entroq.modify.duration"} {
		if !seen[name] {
			t.Errorf("%s not recorded", name)
		}
	}
}
