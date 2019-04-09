// Package pg provides an entroq.Backend using PostgreSQL. Use Opener with
// entroq.New to create a task client that talks to a PostgreSQL backend.
package pg // import "entrogo.com/entroq/pg"

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"time"

	"entrogo.com/entroq"
	"entrogo.com/entroq/subq"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/pkg/errors"
)

func escp(p string) string {
	return strings.NewReplacer("=", "\\=", " ", "\\ ").Replace(p)
}

type pgOptions struct {
	db       string
	user     string
	password string

	attempts int
	nw       entroq.NotifyWaiter
}

// PGOpt sets an option for the opener.
type PGOpt func(opts *pgOptions)

// WithUsername changes the username this database will use to connect.
func WithUsername(name string) PGOpt {
	return func(opts *pgOptions) {
		opts.user = name
	}
}

// WithPassword sets the connection password.
func WithPassword(pwd string) PGOpt {
	return func(opts *pgOptions) {
		opts.password = pwd
	}
}

// WithDB changes the name of the database to connect to.
func WithDB(db string) PGOpt {
	return func(opts *pgOptions) {
		opts.db = db
	}
}

// WithConnectAttempts sets the number of connection attempts before giving up.
// The opener waits 5 seconds between each attempt.
func WithConnectAttempts(num int) PGOpt {
	if num < 1 {
		num = 1
	}
	return func(opts *pgOptions) {
		opts.attempts = num
	}
}

// WithNotifyWaiter instructs this backend to use the given NotifyWaiter
// (instead of its own). This can be useful if there are several interdependent
// postgres backends in the same process space - they can use the same
// notification mechanism.
//
// Can be set to nil to disable internal claim/modify wait/notify and revert to
// claim poll/sleep.
func WithNotifyWaiter(nw entroq.NotifyWaiter) PGOpt {
	return func(opts *pgOptions) {
		opts.nw = nw
	}
}

// Opener creates an opener function to be used to get a backend.
func Opener(hostPort string, opts ...PGOpt) entroq.BackendOpener {
	// Set up some defaults, then apply given options.
	options := &pgOptions{
		db:       "postgres",
		user:     "postgres",
		password: "password",
		attempts: 1,
		nw:       subq.New(),
	}
	for _, o := range opts {
		o(options)
	}

	// TODO: Allow setting of sslmode?
	return func(ctx context.Context) (entroq.Backend, error) {
		connStr := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
			escp(options.user),
			escp(options.password),
			hostPort,
			escp(options.db))
		db, err := sql.Open("postgres", connStr)
		if err != nil {
			return nil, errors.Wrap(err, "failed to open postgres DB")
		}
		for i := 0; i < options.attempts; i++ {
			if err = db.PingContext(ctx); err == nil {
				return New(ctx, db, options.nw)
			}
			if i < options.attempts-1 {
				select {
				case <-ctx.Done():
					return nil, errors.Wrap(ctx.Err(), "pg opener")
				case <-time.After(5 * time.Second):
				}
			}
		}
		return nil, errors.Wrap(err, "time out postgres init")
	}
}

type backend struct {
	db *sql.DB
	nw entroq.NotifyWaiter
}

// New creates a new postgres backend that attaches to the given database.
// If the NotifyWaiter value is provided, Claim will attempt to wait for task
// events, and Modify will notify on changes and insertions that create
// "available" tasks. This allows newly-inserted tasks to be picked up more or
// less immediately if another routine is waiting on the corresponding queue.
//
// Note that this is an *optimization*, not a guarantee that tasks will be
// picked up immediately. It is therefore safe, though not necessarily very
// helpful, for multiple of these backends to have their own NotifyWaiter
// objects.
//
// If left nil, the default behavior is to poll and sleep.
func New(ctx context.Context, db *sql.DB, nw entroq.NotifyWaiter) (*backend, error) {
	b := &backend{
		db: db,
		nw: nw,
	}

	err := b.initDB(ctx)
	if err == io.EOF {
		return nil, io.EOF
	}
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize database")
	}
	return b, nil
}

// Close closes the underlying database connection.
func (b *backend) Close() error {
	return errors.Wrap(b.db.Close(), "pg backend close")
}

// initDB sets up the database to have the appropriate tables and necessary
// extensions to work as a task queue backend.
func (b *backend) initDB(ctx context.Context) error {
	_, err := b.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS tasks (
		  id UUID PRIMARY KEY NOT NULL,
		  version INTEGER NOT NULL DEFAULT 0,
		  queue TEXT NOT NULL DEFAULT '',
		  at TIMESTAMP WITH TIME ZONE NOT NULL,
		  created TIMESTAMP WITH TIME ZONE,
		  modified TIMESTAMP WITH TIME ZONE NOT NULL,
		  claimant UUID,
		  value BYTEA,
		  claims INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS byID ON tasks (id);
		CREATE INDEX IF NOT EXISTS byVersion ON tasks (version);
		CREATE INDEX IF NOT EXISTS byQueue ON tasks (queue);
		CREATE INDEX IF NOT EXISTS byQueueAt ON tasks (queue, at);
	`)
	return errors.Wrap(err, "initDB")
}

// Queues returns a mapping from queue names to task counts within them.
func (b *backend) Queues(ctx context.Context, qq *entroq.QueuesQuery) (map[string]int, error) {
	q := "SELECT queue, COUNT(*) AS count FROM tasks"
	var values []interface{}

	if len(qq.MatchPrefix) != 0 || len(qq.MatchExact) != 0 {
		q += " WHERE"
	}

	var matchFragments []string
	for _, m := range qq.MatchPrefix {
		matchFragments = append(matchFragments, fmt.Sprintf(" queue LIKE $%d", len(values)+1))
		values = append(values, m+"%")
	}
	for _, m := range qq.MatchExact {
		matchFragments = append(matchFragments, fmt.Sprintf(" queue = $%d", len(values)+1))
		values = append(values, m)
	}
	if len(matchFragments) != 0 {
		q += strings.Join(matchFragments, " OR ")
	}

	if qq.Limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", len(values)+1)
		values = append(values, qq.Limit)
	}

	q += " GROUP BY queue"

	rows, err := b.db.QueryContext(ctx, q, values...)
	if err != nil {
		return nil, errors.Wrap(err, "queue names")
	}

	defer rows.Close()
	queues := make(map[string]int)
	for rows.Next() {
		var (
			q     string
			count int
		)
		if err := rows.Scan(&q, &count); err != nil {
			return nil, errors.Wrap(err, "row scan")
		}
		queues[q] = count
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "queue iteration")
	}
	return queues, nil
}

// Tasks returns a slice of all tasks in the given queue.
func (b *backend) Tasks(ctx context.Context, tq *entroq.TasksQuery) ([]*entroq.Task, error) {
	q := "SELECT id, version, queue, at, created, modified, claimant, value FROM tasks WHERE queue = $1"
	values := []interface{}{tq.Queue}

	if tq.Claimant != uuid.Nil {
		q += fmt.Sprintf(" AND (claimant = $%d OR claimant = $%d OR at < NOW())", len(values)+1, len(values)+2)
		values = append(values, uuid.Nil.String(), tq.Claimant)
	}

	// Add IDs if a set of limiting IDs has been requested.
	if len(tq.IDs) > 0 {
		var specifiers []string
		for _, id := range tq.IDs {
			specifiers = append(specifiers, fmt.Sprintf("$%d", len(values)+1))
			values = append(values, id.String())
		}
		q += " AND id IN (" + strings.Join(specifiers, ", ") + ")"
	}

	if tq.Limit > 0 {
		// Safe to directly append, since it's an int.
		q += fmt.Sprintf(" LIMIT %d", tq.Limit)
	}

	rows, err := b.db.QueryContext(ctx, q, values...)
	if err != nil {
		return nil, errors.Wrapf(err, "queue tasks %q", tq.Queue)
	}
	defer rows.Close()
	var tasks []*entroq.Task
	for rows.Next() {
		t := &entroq.Task{}
		if err := rows.Scan(&t.ID, &t.Version, &t.Queue, &t.At, &t.Created, &t.Modified, &t.Claimant, &t.Value); err != nil {
			return nil, errors.Wrap(err, "task scan")
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Wrapf(err, "queue task iteration %q", tq.Queue)
	}
	return tasks, nil
}

// Claim attempts to claim an arrived task from the queue, and blocks if
// something goes wrong.
func (b *backend) Claim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	if b.nw != nil {
		return entroq.WaitTryClaim(ctx, cq, b.TryClaim, b.nw)
	}
	return entroq.PollTryClaim(ctx, cq, b.TryClaim)
}

// TryClaim attempts to claim an "arrived" task from the queue.
// Returns an error if something goes wrong, a nil task if there is
// nothing to claim.
func (b *backend) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	task := new(entroq.Task)
	if cq.Duration == 0 {
		return nil, errors.Errorf("no duration set for claim %q", cq.Queue)
	}
	now := time.Now()
	err := b.db.QueryRowContext(ctx, `
		WITH topN AS (
			SELECT * FROM tasks
			WHERE
				queue = $1 AND
				at <= $2
			ORDER BY at, version, id ASC
			LIMIT 20
			FOR UPDATE
		)
		UPDATE tasks
		SET
			version = version + 1,
			claims = claims + 1,
			at = $3,
			claimant = $4,
			modified = $5
		WHERE id IN (SELECT id FROM topN ORDER BY RANDOM() LIMIT 1)
		RETURNING id, version, queue, at, created, modified, claimant, value, claims
	`, cq.Queue, now, now.Add(cq.Duration), cq.Claimant, now).Scan(
		&task.ID,
		&task.Version,
		&task.Queue,
		&task.At,
		&task.Created,
		&task.Modified,
		&task.Claimant,
		&task.Value,
		&task.Claims,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, errors.Wrap(err, "pg try claim")
	}
	return task, nil
}

func isSerialization(err error) bool {
	if err == nil {
		return false
	}
	e, ok := errors.Cause(err).(*pq.Error)
	return ok && e.Code == "40001"
}

// Modify attempts to apply an atomic modification to the task store. Either
// all succeeds or all fails.
func (b *backend) Modify(ctx context.Context, mod *entroq.Modification) (inserted, changed []*entroq.Task, err error) {
	for i := 0; i < 7; i++ {
		inserted, changed, err = b.modify(ctx, mod)
		// No error - we're done!
		if err == nil {
			// Notify any waiters of tasks that were just changed/inserted that are
			// ready to go.
			if b.nw != nil {
				entroq.NotifyModified(b.nw, inserted, changed)
			}
			return inserted, changed, nil
		}
		if _, ok := entroq.AsDependency(err); ok {
			// We know what's wrong, no reason to retry.
			return nil, nil, errors.Wrap(err, "pg modify dependency")
		}
		if !isSerialization(err) {
			// We didn't get a retriable serialization error, return.
			return nil, nil, errors.Wrap(err, "pg modify unknown")
		}
		// Serialization error - go around again.
		time.Sleep(time.Second)
	}
	// Serialization errors that can't be retried are passed as empty
	// dependency errors. We don't know what the conflict was, but it was like
	// a dependency problem.
	return nil, nil, entroq.DependencyErrorf("retry limit: %v", err)
}

// modify is a private helper to allow for transaction retries when serialization is violated.
func (b *backend) modify(ctx context.Context, mod *entroq.Modification) (inserted, changed []*entroq.Task, err error) {
	now := time.Now()
	tx, err := b.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to start transaction")
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()

	foundDeps, err := depQuery(ctx, tx, mod)
	if err != nil {
		return nil, nil, errors.Wrap(err, "get deps")
	}

	if err := mod.DependencyError(foundDeps); err != nil {
		return nil, nil, errors.Wrap(err, "dependency issue found")
	}

	// Once we get here, we know that all of the dependencies were present.
	// These should now all succeed.

	for _, tid := range mod.Deletes {
		q := "DELETE FROM tasks WHERE id = $1 AND version = $2"
		if _, err := tx.ExecContext(ctx, q, tid.ID, tid.Version); err != nil {
			return nil, nil, errors.Wrapf(err, "pg modify delete %s", tid)
		}
	}

	for _, ins := range mod.Inserts {
		at := ins.At
		if at.IsZero() {
			at = now
		}

		id := ins.ID
		if id == uuid.Nil {
			id = uuid.New()
		}

		t := new(entroq.Task)
		row := tx.QueryRowContext(ctx, `
			INSERT INTO tasks (id, version, queue, at, claimant, value, created, modified)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id, version, queue, at, claimant, value, created, modified
		`, id, 0, ins.Queue, at, mod.Claimant, ins.Value, now, now)
		if err := row.Scan(&t.ID, &t.Version, &t.Queue, &t.At, &t.Claimant, &t.Value, &t.Created, &t.Modified); err != nil {
			return nil, nil, errors.Wrap(err, "pg modify insert scan")
		}
		inserted = append(inserted, t)
	}

	for _, chg := range mod.Changes {
		t := new(entroq.Task)
		row := tx.QueryRowContext(ctx, `
			UPDATE tasks SET version = version + 1, modified = $1, queue = $2, at = $3, value = $4
			WHERE id = $5 AND version = $6
			RETURNING id, version, queue, at, claimant, modified, created, value
		`, now, chg.Queue, chg.At, chg.Value, chg.ID, chg.Version)
		if err := row.Scan(&t.ID, &t.Version, &t.Queue, &t.At, &t.Claimant, &t.Modified, &t.Created, &t.Value); err != nil {
			return nil, nil, errors.Wrapf(err, "pg modify update scan %s", chg.ID)
		}
		changed = append(changed, t)
	}

	return inserted, changed, nil
}

// depQuery sends a query to the database, within a transaction, that will lock
// all rows for all dependencies of a modification, allowing updates to be
// performed on those rows without other transactions getting in the middle of
// things. Returns a map from ID to version for every dependency that is found,
// or an error if the query itself fails.
func depQuery(ctx context.Context, tx *sql.Tx, m *entroq.Modification) (map[uuid.UUID]*entroq.Task, error) {
	// We must craft a query that ensures that changes, deletes, and depends
	// all exist with the right versions, and then insert all inserts, delete
	// all deletes, and update all changes.
	dependencies, err := m.AllDependencies()
	if err != nil {
		return nil, errors.Wrap(err, "pg dep query")
	}

	foundDeps := make(map[uuid.UUID]*entroq.Task)

	if len(dependencies) == 0 {
		return foundDeps, nil
	}

	// Form a SELECT FOR UPDATE that looks at all of these rows so that we can
	// touch them all while inserting, deleting, and changing, and so we can
	// guarantee that dependencies don't disappear while we're at it (we need
	// to know that the dependencies are there while we work).
	var placeholders []string
	var values []interface{}
	for id := range dependencies {
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(values)+1))
		values = append(values, id)
	}
	query := `SELECT id, version, queue, at, claimant, value, created, modified FROM tasks
	          WHERE id IN (` + strings.Join(placeholders, ", ") + `) FOR UPDATE`
	rows, err := tx.QueryContext(ctx, query, values...)
	if err != nil {
		return nil, errors.Wrap(err, "pg dep query")
	}
	defer rows.Close()
	for rows.Next() {
		t := new(entroq.Task)
		if err := rows.Scan(&t.ID, &t.Version, &t.Queue, &t.At, &t.Claimant, &t.Value, &t.Created, &t.Modified); err != nil {
			return nil, errors.Wrap(err, "pg dep scan")
		}
		if foundDeps[t.ID] != nil {
			return nil, errors.Errorf("pg duplicate ID %q found", t.ID)
		}
		foundDeps[t.ID] = t
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "pg dep iteration")
	}

	return foundDeps, nil
}
