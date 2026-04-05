package eqpg

import (
	"context"
	"fmt"
	"testing"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/backend/eqtest"
)

// populateRaw handles high-speed setup using PostgreSQL's generate_series.
func populateRaw(ctx context.Context, backend *EQPG, n int, queues []string) error {
	if len(queues) == 0 {
		return nil
	}
	const q = `
		INSERT INTO entroq.tasks (id, version, queue, at, created, modified, claimant, value, claims, attempt, err)
		SELECT
			gen_random_uuid(),
			0,
			$1,
			now(),
			now(),
			now(),
			'',
			'bench-value'::bytea,
			0, 0, ''
		FROM generate_series(1, $2)
	`
	for _, queue := range queues {
		perQueue := n / len(queues)
		if perQueue == 0 {
			perQueue = 1
		}
		if _, err := backend.DB.ExecContext(ctx, q, queue, perQueue); err != nil {
			return fmt.Errorf("populate queue %q: %w", queue, err)
		}
	}
	return nil
}

func BenchmarkStatsContention_1M(b *testing.B) {
	ctx := context.Background()
	queues := []string{"/bench/q1", "/bench/q2", "/bench/q3", "/bench/prefix/a", "/bench/prefix/b"}

	backend, err := Open(ctx, pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(10))
	if err != nil {
		b.Fatalf("Failed to open backend: %v", err)
	}
	defer backend.Close()

	if err := populateRaw(ctx, backend, 1000000, queues); err != nil {
		b.Fatalf("Populate failed: %v", err)
	}

	client, err := entroq.New(ctx, nil, entroq.WithBackend(backend))
	if err != nil {
		b.Fatalf("Failed to create client: %v", err)
	}

	eqtest.RunContentionBenchmark(b, client, queues, 20)
}

func BenchmarkStatsContention_1M_PollOnly(b *testing.B) {
	ctx := context.Background()
	queues := []string{"/bench/q1", "/bench/q2", "/bench/q3", "/bench/prefix/a", "/bench/prefix/b"}

	backend, err := Open(ctx, pgHostPort,
		WithDB("postgres"),
		WithUsername("postgres"),
		WithPassword("password"),
		WithConnectAttempts(10))
	if err != nil {
		b.Fatalf("Failed to open backend: %v", err)
	}
	defer backend.Close()

	if err := populateRaw(ctx, backend, 1000000, queues); err != nil {
		b.Fatalf("Populate failed: %v", err)
	}

	client, err := entroq.New(ctx, nil, entroq.WithBackend(backend))
	if err != nil {
		b.Fatalf("Failed to create client: %v", err)
	}

	eqtest.RunContentionBenchmark(b, client, queues, 20)
}
