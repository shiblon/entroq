package eqsqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"slices"
	"time"

	"github.com/shiblon/entroq"
)

const claimWindow = 64

// TryClaim attempts to claim an arrived task from any of the specified queues.
// It returns a nil task without an error if no task is available.
func (b *EQSQLite) TryClaim(ctx context.Context, q *entroq.ClaimQuery) (*entroq.Task, error) {
	start := time.Now()
	defer func() { b.claimDur.Record(ctx, time.Since(start).Seconds()) }()
	if err := validateClaim(q); err != nil {
		return nil, fmt.Errorf("eqsqlite try claim: %w", err)
	}
	queues := slices.Clone(q.Queues)
	rand.Shuffle(len(queues), func(i, j int) { queues[i], queues[j] = queues[j], queues[i] })
	value, err := b.write(ctx, func(ctx context.Context, tx *sql.Tx) (any, error) {
		now := nowUTC()
		for _, queue := range queues {
			row := tx.QueryRowContext(ctx, `
                    WITH head AS MATERIALIZED (
                        SELECT rowid FROM tasks
                        WHERE queue = ? AND at_ms <= ?
                        ORDER BY at_ms, id
                        LIMIT ?
                    )
                    UPDATE tasks
                    SET version = version + 1,
                        at_ms = ?, claimant = ?, claims = claims + 1,
                        modified_ms = ?
                    WHERE rowid = (SELECT rowid FROM head ORDER BY random() LIMIT 1)
                    RETURNING `+taskColumns,
				queue, now.UnixMilli(), claimWindow,
				now.Add(q.Duration).UnixMilli(), q.Claimant, now.UnixMilli())
			task, err := scanTask(row)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("claim queue %q: %w", queue, err)
			}
			return task, nil
		}
		return nil, nil
	})
	if err != nil {
		return nil, fmt.Errorf("eqsqlite try claim: %w", err)
	}
	if value == nil {
		return nil, nil
	}
	return value.(*entroq.Task), nil
}

// Claim waits for an arrived task to become available, then claims it.
func (b *EQSQLite) Claim(ctx context.Context, q *entroq.ClaimQuery) (*entroq.Task, error) {
	if err := validateClaim(q); err != nil {
		return nil, fmt.Errorf("eqsqlite claim: %w", err)
	}
	task, err := b.TryClaim(ctx, q)
	if err != nil || task != nil {
		return task, err
	}
	return entroq.WaitTryClaim(ctx, q, b.tryClaimWhenReady, b.nw)
}

func validateClaim(q *entroq.ClaimQuery) error {
	if q == nil || len(q.Queues) == 0 {
		return fmt.Errorf("no queues")
	}
	if q.Duration == 0 {
		return fmt.Errorf("zero duration")
	}
	return nil
}

// tryClaimWhenReady keeps empty blocking-claim polls out of the write pool.
// A positive probe is only a hint: TryClaim remains the atomic decision.
func (b *EQSQLite) tryClaimWhenReady(ctx context.Context, q *entroq.ClaimQuery) (*entroq.Task, error) {
	args := make([]any, 0, len(q.Queues)+1)
	for _, queue := range q.Queues {
		args = append(args, queue)
	}
	args = append(args, nowUTC().UnixMilli())
	query := "SELECT EXISTS(SELECT 1 FROM tasks WHERE queue IN (" + placeholders(len(q.Queues)) + ") AND at_ms <= ? LIMIT 1)"
	var ready int
	if err := b.readDB.QueryRowContext(ctx, query, args...).Scan(&ready); err != nil {
		return nil, fmt.Errorf("eqsqlite claim readiness: %w", err)
	}
	if ready == 0 {
		return nil, nil
	}
	return b.TryClaim(ctx, q)
}
