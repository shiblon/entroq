package pg

import (
	"context"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"entrogo.com/entroq"
	"entrogo.com/entroq/contrib/mrtest"
	"entrogo.com/entroq/qsvc/qtest"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"

	_ "github.com/lib/pq"
)

var pgPort int

func TestMain(m *testing.M) {
	ctx := context.Background()

	var (
		err    error
		pgStop func()
	)
	if pgPort, pgStop, err = startPostgres(ctx); err != nil {
		log.Fatalf("Postgres start: %v", err)
	}

	res := m.Run()
	pgStop()

	os.Exit(res)
}

func pgClient(ctx context.Context) (client *entroq.EntroQ, stop func(), err error) {
	return qtest.ClientService(ctx, Opener(fmt.Sprintf("localhost:%v", pgPort),
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(10)))
}

func RunQTest(t *testing.T, tester qtest.Tester) {
	t.Helper()
	ctx := context.Background()
	client, stop, err := pgClient(ctx)
	if err != nil {
		t.Fatalf("Get client: %v", err)
	}
	defer stop()
	tester(ctx, t, client, "pgtest/"+uuid.New().String())
}

func TestTasksWithID(t *testing.T) {
	RunQTest(t, qtest.TasksWithID)
}

func TestTasksOmitValue(t *testing.T) {
	RunQTest(t, qtest.TasksOmitValue)
}

func TestTasksWithIDOnly(t *testing.T) {
	RunQTest(t, qtest.TasksWithIDOnly)
}

func TestInsertWithID(t *testing.T) {
	RunQTest(t, qtest.InsertWithID)
}

func TestSimpleSequence(t *testing.T) {
	RunQTest(t, qtest.SimpleSequence)
}

func TestSimpleChange(t *testing.T) {
	log.Printf("Simple change")
	RunQTest(t, qtest.SimpleChange)
}

func TestSimpleWorker(t *testing.T) {
	log.Printf("Simple worker")
	RunQTest(t, qtest.SimpleWorker)
}

func TestMultiWorker(t *testing.T) {
	log.Printf("Multi worker")
	RunQTest(t, qtest.MultiWorker)
}

func TestWorkerDependencyHandler(t *testing.T) {
	log.Printf("Worker dependency handler")
	RunQTest(t, qtest.WorkerDependencyHandler)
}

func TestWorkerMoveOnError(t *testing.T) {
	log.Printf("Worker move on error")
	RunQTest(t, qtest.WorkerMoveOnError)
}

func TestWorkerRetryOnError(t *testing.T) {
	log.Printf("Worker retry on error")
	RunQTest(t, qtest.WorkerRetryOnError)
}

func TestWorkerRenewal(t *testing.T) {
	log.Printf("Worker renewal")
	RunQTest(t, qtest.WorkerRenewal)
}

func TestQueueMatch(t *testing.T) {
	RunQTest(t, qtest.QueueMatch)
}

func TestQueueStats(t *testing.T) {
	RunQTest(t, qtest.QueueStats)
}

func TestMapReduce_checkTiny(t *testing.T) {
	ctx := context.Background()
	client, stop, err := pgClient(ctx)
	if err != nil {
		t.Fatalf("Open pg client: %v", err)
	}
	defer stop()

	config := &quick.Config{
		MaxCount: 2,
		Values: func(values []reflect.Value, rand *rand.Rand) {
			values[0] = reflect.ValueOf(5)
			values[1] = reflect.ValueOf(rand.Intn(2) + 1)
			values[2] = reflect.ValueOf(1)
		},
	}

	check := func(ndocs, nm, nr int) bool {
		return mrtest.MRCheck(ctx, client, ndocs, nm, nr)
	}
	if err := quick.Check(check, config); err != nil {
		t.Fatal(err)
	}
}

func TestMapReduce_checkSmall(t *testing.T) {
	ctx := context.Background()
	client, stop, err := pgClient(ctx)
	if err != nil {
		t.Fatalf("Open pg client: %v", err)
	}
	defer stop()

	config := &quick.Config{
		MaxCount: 3,
		Values: func(values []reflect.Value, rand *rand.Rand) {
			values[0] = reflect.ValueOf(rand.Intn(500) + 100)
			values[1] = reflect.ValueOf(rand.Intn(30) + 10)
			values[2] = reflect.ValueOf(rand.Intn(10) + 1)
		},
	}
	check := func(ndocs, nm, nr int) bool {
		return mrtest.MRCheck(ctx, client, ndocs, nm, nr)
	}
	if err := quick.Check(check, config); err != nil {
		t.Fatal(err)
	}
}

// run starts a subprocess and connects its standard pipes to the parents'.
func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)

	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return errors.Wrapf(err, "open stdout %q %q", name, args)
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return errors.Wrapf(err, "open stderr %q %q", name, args)
	}

	if err := cmd.Start(); err != nil {
		return errors.Wrapf(err, "start %q %q", name, args)
	}

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		if _, err := io.Copy(os.Stdout, outPipe); err != nil {
			return errors.Wrap(err, "stdout copy")
		}
		return nil
	})

	g.Go(func() error {
		if _, err := io.Copy(os.Stderr, errPipe); err != nil {
			return errors.Wrap(err, "stderr copy")
		}
		return nil
	})

	g.Go(cmd.Wait)

	return g.Wait()
}

// startPostgres starts up a postgres docker. Use the stop function to stop it.
func startPostgres(ctx context.Context) (port int, stop func(), err error) {
	// Run detached. Check error. Detaches only once Postgres is downloaded and initialized.
	name := fmt.Sprintf("testpg-%s", uuid.New())

	log.Printf("Starting postgres container %q...", name)
	if err := run(ctx, "docker", "run", "-p", "0:5432", "--rm", "-d", "-e", "POSTGRES_PASSWORD=password", "--name", name, "postgres:11"); err != nil {
		return 0, nil, errors.Wrap(err, "start postgres container")
	}
	time.Sleep(2 * time.Second) // give it some time to get pipes attached
	log.Print("Container is up")

	stopFunc := func() {
		log.Printf("Stopping postgres container %q...", name)
		if err := run(ctx, "docker", "container", "stop", name); err != nil {
			log.Printf("Error stopping: %v", err)
			return
		}
		log.Printf("Container is down")
	}

	defer func() {
		if err != nil {
			log.Println("Error initializing postgres container")
			stopFunc()
		}
	}()

	// Now the container is running. Get its port.
	portOut, err := exec.CommandContext(ctx, "docker", "inspect", "-f", `{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}`, name).Output()
	if err != nil {
		return 0, nil, errors.Wrap(err, "unable to get port")
	}
	port, err = strconv.Atoi(strings.TrimSpace(string(portOut)))
	if err != nil {
		return 0, nil, errors.Wrapf(err, "failed to parse port number %q", portOut)
	}

	return port, stopFunc, nil
}
