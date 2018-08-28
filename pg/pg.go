// Package pg provides an entroq.Backend using PostgreSQL. Use Opener with
// entroq.New to create a task client that talks to a PostgreSQL backend.
package pg

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shiblon/entroq"
)

func escp(p string) string {
	return strings.NewReplacer("=", "\\=", " ", "__").Replace(p)
}

type pgOptions struct {
	db       string
	user     string
	password string
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

// Opener creates an opener function to be used to get a backend.
func Opener(hostPort string, opts ...PGOpt) entroq.BackendOpener {
	// Set up some defaults, then apply given options.
	options := &pgOptions{
		db:       "postgres",
		user:     "postgres",
		password: "postgres",
	}
	for _, o := range opts {
		o(options)
	}

	// TODO: allow setting of sslmode?
	return func(ctx context.Context) (entroq.Backend, error) {
		connStr := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
			escp(options.user),
			escp(options.password),
			hostPort,
			escp(options.db))
		db, err := sql.Open("postgres", connStr)
		if err != nil {
			return nil, fmt.Errorf("failed to open postgres DB: %v", err)
		}
		return New(ctx, db)
	}
}

type backend struct {
	db *sql.DB
}

// New creates a new postgres backend that attaches to the given database.
func New(ctx context.Context, db *sql.DB) (*backend, error) {
	b := &backend{db: db}

	if err := b.initDB(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize database: %v", err)
	}

	return b, nil
}

// Close closes the underlying database connection.
func (b *backend) Close() error {
	return b.db.Close()
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
		  value BYTEA
		);
		CREATE INDEX IF NOT EXISTS byQueue ON tasks (queue);
		CREATE INDEX IF NOT EXISTS byQueueAt ON tasks (queue, at);
	`)
	if err != nil {
		return fmt.Errorf("failed to create (or reuse) tasks table: %v", err)
	}
	return nil
}

// Queues returns a mapping from queue names to task counts within them.
func (b *backend) Queues(ctx context.Context) (map[string]int, error) {
	rows, err := b.db.QueryContext(ctx,
		"SELECT queue, COUNT(*) AS count FROM tasks GROUP BY queue")
	if err != nil {
		return nil, fmt.Errorf("failed to get queue names: %v", err)
	}
	defer rows.Close()
	queues := make(map[string]int)
	for rows.Next() {
		var (
			q     string
			count int
		)
		if err := rows.Scan(&q, &count); err != nil {
			return nil, fmt.Errorf("queue scan failed: %v", err)
		}
		queues[q] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue iteration failed: %v", err)
	}
	return queues, nil
}

// Tasks returns a slice of all tasks in the given queue.
func (b *backend) Tasks(ctx context.Context, tq *entroq.TasksQuery) ([]*entroq.Task, error) {
	values := []interface{}{tq.Queue}
	q := "SELECT id, version, queue, at, created, modified, claimant, value FROM tasks WHERE queue = $1"

	if tq.Claimant != uuid.Nil {
		q += " AND (claimant = $2 OR claimant = $3 OR at < NOW())"
		values = append(values, uuid.Nil.String(), tq.Claimant)
	}

	rows, err := b.db.QueryContext(ctx, q, values...)
	if err != nil {
		return nil, fmt.Errorf("failed to get tasks for queue %q: %v", tq.Queue, err)
	}
	defer rows.Close()
	var tasks []*entroq.Task
	for rows.Next() {
		t := &entroq.Task{}
		if err := rows.Scan(&t.ID, &t.Version, &t.Queue, &t.At, &t.Created, &t.Modified, &t.Claimant, &t.Value); err != nil {
			return nil, fmt.Errorf("task scan failed: %v", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("task iteration failed for queue %q: %v", tq.Queue, err)
	}
	return tasks, nil
}

// TryClaim attempts to claim an "arrived" task from the queue.
// Returns an error if something goes wrong, a nil task if there is
// nothing to claim.
func (b *backend) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	task := new(entroq.Task)
	if cq.Duration == 0 {
		return nil, fmt.Errorf("no duration set for claim")
	}
	now := time.Now()
	err := b.db.QueryRowContext(ctx, `
		WITH topN AS (
			SELECT * FROM tasks
			WHERE
				queue = $1 AND
				at <= $2
			ORDER BY at, version, id ASC
			LIMIT 10
		)
		UPDATE tasks
		SET
			version=version+1,
			at = $3,
			claimant = $4,
			modified = $5
		WHERE id IN (SELECT id FROM topN ORDER BY random() LIMIT 1)
		RETURNING id, version, queue, at, created, modified, claimant, value
	`, cq.Queue, now, now.Add(cq.Duration), cq.Claimant, now).Scan(
		&task.ID,
		&task.Version,
		&task.Queue,
		&task.At,
		&task.Created,
		&task.Modified,
		&task.Claimant,
		&task.Value,
	)
	if err != nil {
		if err != sql.ErrNoRows {
			return nil, err
		}
		return nil, nil
	}
	return task, nil
}

// Modify attempts to apply an atomic modification to the task store. Either
// all succeeds or all fails.
func (b *backend) Modify(ctx context.Context, mod *entroq.Modification) (inserted []*entroq.Task, changed []*entroq.Task, err error) {
	now := time.Now()
	tx, err := b.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to start transaction: %v", err)
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
		return nil, nil, fmt.Errorf("failed to get dependencies: %v", err)
	}

	if err := mod.DependencyError(foundDeps); err != nil {
		return nil, nil, err
	}

	// Once we get here, we know that all of the dependencies were present.
	// These should now all succeed.

	for _, tid := range mod.Deletes {
		q := "DELETE FROM tasks WHERE id = $1 AND version = $2"
		if _, err := tx.ExecContext(ctx, q, tid.ID, tid.Version); err != nil {
			return nil, nil, fmt.Errorf("delete failed for task %q: %v", tid.ID, err)
		}
	}

	for _, ins := range mod.Inserts {
		at := ins.At
		if at.IsZero() {
			at = now
		}

		t := new(entroq.Task)
		row := tx.QueryRowContext(ctx, `
			INSERT INTO tasks (id, version, queue, at, claimant, value, created, modified)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id, version, queue, at, claimant, value, created, modified
		`, uuid.New(), 0, ins.Queue, at, mod.Claimant, ins.Value, now, now)
		if err := row.Scan(&t.ID, &t.Version, &t.Queue, &t.At, &t.Claimant, &t.Value, &t.Created, &t.Modified); err != nil {
			return nil, nil, fmt.Errorf("insert failed in queue %q: %v", ins.Queue, err)
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
			return nil, nil, fmt.Errorf("scan failed for newly-changed task %q: %v", chg.ID, err)
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
		return nil, err
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
	first := 1
	for id, version := range dependencies {
		placeholders = append(placeholders, fmt.Sprintf("($%d, $%d)", first, first+1))
		values = append(values, id, version)
		first++
	}
	instr := strings.Join(placeholders, ", ")
	rows, err := tx.QueryContext(ctx,
		"SELECT id, version, queue, at, claimant, value, created, modified FROM tasks WHERE (id, version) IN ("+instr+") FOR UPDATE",
		values...)
	if err != nil {
		return nil, fmt.Errorf("error in dependencies query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		t := new(entroq.Task)
		if err := rows.Scan(&t.ID, &t.Version, &t.Queue, &t.At, &t.Claimant, &t.Value, &t.Created, &t.Modified); err != nil {
			return nil, fmt.Errorf("row scan failed: %v", err)
		}
		if foundDeps[t.ID] != nil {
			return nil, fmt.Errorf("duplicate ID %q found in database (more than one version)", t.ID)
		}
		foundDeps[t.ID] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error scanning dependencies: %v", err)
	}

	return foundDeps, nil
}
