package pg

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/shiblon/entroq"
	grpcbackend "github.com/shiblon/entroq/grpc"
	"github.com/shiblon/entroq/qsvc/qtest"
	"golang.org/x/sync/errgroup"

	_ "github.com/lib/pq"
)

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)

	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open stdout %q %q: %v", name, args, err)
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open stderr %q %q: %v", name, args, err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %q %q: %v", name, args, err)
	}

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		if _, err := io.Copy(os.Stdout, outPipe); err != nil {
			return fmt.Errorf("stdout copy: %v", err)
		}
		return nil
	})

	g.Go(func() error {
		if _, err := io.Copy(os.Stderr, errPipe); err != nil {
			return fmt.Errorf("stderr copy: %v", err)
		}
		return nil
	})

	g.Go(cmd.Wait)

	return g.Wait()
}

// startPostgres starts up a postgres docker. Use the stop function to stop it.
func startPostgres(ctx context.Context) (port int, stop func(), err error) {
	// Run detached. Check error. Detaches only once Postgres is downloaded and initialized.
	name := fmt.Sprintf("testpg-%d", os.Getpid())
	log.Printf("Starting postgres container %q...", name)
	if err := run(ctx, "docker", "run", "-p", "0:5432", "-d", "--name", name, "postgres"); err != nil {
		return 0, nil, fmt.Errorf("start postgres container: %v", err)
	}
	log.Print("Success")

	stopFunc := func() {
		log.Printf("Stopping postgres container %q...", name)
		if err := run(ctx, "docker", "container", "stop", name); err != nil {
			log.Printf("Error stopping: %v", err)
			return
		}
		log.Printf("Success")
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
		return 0, nil, fmt.Errorf("unable to get port: %v", err)
	}
	port, err = strconv.Atoi(strings.TrimSpace(string(portOut)))
	if err != nil {
		return 0, nil, fmt.Errorf("failed to parse port number %q: %v", portOut, err)
	}

	return port, stopFunc, nil
}

func TestSimpleSequence(t *testing.T) {
	ctx := context.Background()

	pgPort, pgStop, err := startPostgres(ctx)
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	defer pgStop()

	pgHostPort := fmt.Sprintf("localhost:%v", pgPort)

	server, dial, err := qtest.StartService(ctx, Opener(pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(3)))
	if err != nil {
		t.Fatalf("Could not start service: %v", err)
	}
	defer server.Stop()

	client, err := entroq.New(ctx, grpcbackend.Opener("bufnet",
		grpcbackend.WithNiladicDialer(dial),
		grpcbackend.WithInsecure()))
	if err != nil {
		t.Fatalf("Open client: %v", err)
	}
	defer client.Close()

	qtest.SimpleSequence(ctx, t, client)
}
