package eqmr_test

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/backend/eqpg"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// backendEnv selects the backend newClient opens: unset or "mem" for eqmem,
// "pg" for PostgreSQL in a testcontainer, which needs Docker.
const backendEnv = "EQMR_TEST_BACKEND"

// opener opens the backend for newClient, set by TestMain.
var opener entroq.BackendOpener = eqmem.Opener()

func TestMain(m *testing.M) {
	switch b := os.Getenv(backendEnv); b {
	case "", "mem":
		os.Exit(m.Run())
	case "pg":
		os.Exit(runOnPostgres(m))
	default:
		log.Fatalf("%s=%q: want mem or pg", backendEnv, b)
	}
}

func runOnPostgres(m *testing.M) int {
	ctx := context.Background()
	ctr, err := postgres.Run(ctx, "postgres:17",
		postgres.WithPassword("password"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		log.Fatalf("start Postgres: %v", err)
	}
	defer func() {
		if err := ctr.Terminate(ctx); err != nil {
			log.Printf("stop Postgres: %v", err)
		}
	}()
	hostPort, err := ctr.Endpoint(ctx, "")
	if err != nil {
		log.Fatalf("Postgres endpoint: %v", err)
	}
	opts := []eqpg.PGOpt{
		eqpg.WithDB("postgres"),
		eqpg.WithUsername("postgres"),
		eqpg.WithPassword("password"),
		eqpg.WithConnectAttempts(10),
	}
	back, err := eqpg.Open(ctx, hostPort, append(opts, eqpg.WithInitSchema())...)
	if err != nil {
		log.Fatalf("init Postgres schema: %v", err)
	}
	back.Close()
	opener = eqpg.Opener(hostPort, opts...)
	return m.Run()
}
