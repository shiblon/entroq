// Package pgtask provides a client to a task store using Postgres as a backend.
package pgtask

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	uuid "github.com/google/uuid"

	_ "github.com/lib/pq"
)

// initDB sets up the database to have the appropriate tables and necessary
// extensions to work as a task queue backend.
func initDB(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
		CREATE TABLE IF NOT EXISTS tasks (
		  id UUID PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
		  version INTEGER NOT NULL DEFAULT 0,
		  queue TEXT NOT NULL DEFAULT '',
		  at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
		  created TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
		  modified TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
		  claimant UUID,
		  value BYTEA
		);
		CREATE INDEX IF NOT EXISTS byQueue ON tasks (queue);
		CREATE INDEX IF NOT EXISTS byQueueAT ON tasks (queue, at);
	`)
	if err != nil {
		return fmt.Errorf("failed to create (or reuse) tasks table: %v", err)
	}
	return nil
}

// taskID contains the identifying parts of a task. If IDs don't match
// (identifier and version together), then operations fail on those tasks.
type taskID struct {
	id      string
	version int32
}

// taskData contains just the data, not the identifier or metadata. Used for insertions.
type taskData struct {
	queue string
	at    time.Time
	value []byte
}

// Task represents a unit of work, with a byte slice value payload.
type Task struct {
	queue string

	id      string
	version int32

	at       time.Time
	claimant uuid.UUID
	value    []byte

	created  time.Time
	modified time.Time
}

// Queue returns the queue name for this task.
func (t *Task) Queue() string { return t.queue }

// ID returns the ID of this task (not including the version, which is also part of its key).
func (t *Task) ID() string { return t.id }

// Version returns the version of this task (unique when paired with ID).
func (t *Task) Version() int32 { return t.version }

// AT returns the arrival time of this task.
func (t *Task) AT() time.Time { return t.at }

// Claimant returns the ID of the owner of this task, if not expired.
func (t *Task) Claimant() uuid.UUID { return t.claimant }

// Value returns the value of this task.
func (t *Task) Value() []byte { return t.value }

// Created returns the creation time of this task.
func (t *Task) Created() time.Time { return t.created }

// Modified returns the modification time of this task.
func (t *Task) Modified() time.Time { return t.modified }

// AsDelete returns a ModifyArg that can be used in the Modify function, e.g.,
//
//   cli.Modify(ctx, task1.AsDelete())
//
// The above would cause the given task to be deleted, if it can be. It is
// shorthand for
//
//   cli.Modify(ctx, Deleting(task1.ID(), task1.Version()))
func (t *Task) AsDelete() ModifyArg {
	return Deleting(t.id, t.version)
}

// AsChange returns a ModifyArg that can be used in the Modify function, e.g.,
//
//   cli.Modify(ctx, task1.AsChange(ArrivalTimeBy(2 * time.Minute)))
//
// The above is shorthand for
//
//   cli.Modify(ctx, Changing(task1, ArrivalTimeBy(2 * time.Minute)))
func (t *Task) AsChange(args ...ChangeArg) ModifyArg {
	return Changing(t, args...)
}

// AsDependency returns a ModifyArg that can be used to create a Modify dependency, e.g.,
//
//   cli.Modify(ctx, task.AsDependency())
//
// That is shorthand for
//
//   cli.Modify(ctx, DependingOn(task.ID(), task.Version()))
func (t *Task) AsDependency() ModifyArg {
	return DependingOn(t.ID(), t.Version())
}

// Client is a client interface for accessing tasks implemented in PostgreSQL.
type Client struct {
	db *sql.DB

	dbName   string
	username string
	sslMode  string
	password string

	claimant uuid.UUID
}

var escapePattern = regexp.MustCompile(`(['" \\])`)

func escapeOptVal(v string) string {
	return "'" + escapePattern.ReplaceAllString(v, "\\$1") + "'"
}

const (
	DefaultPassword = "password" // default postgres password
	DefaultUsername = "postgres" // default postgres username
	DefaultDBName   = "entroq"   // default postgres database
	DefaultSSLMode  = "disable"  // default postgres SSL mode
)

// NewClient creates a new task client with the given options. Options
// are created using package-level functions that produce a ClientOption, e.g.,
//
//   cli, err := NewClient(
//   	WithDBName("postgres"),
//   	WithUsername("myuser"),
//   	WithPassword("thepassword"),
//   	WithClaimant(uuid.New().String()),
//   )
//
// Note that there are defaults for all of these fields, specified as constants.
// The default claimant value is a new random UUID, created in NewClient.
func NewClient(opts ...ClientOption) (*Client, error) {
	client := &Client{
		claimant: uuid.New(),
		username: DefaultUsername,
		dbName:   DefaultDBName,
		sslMode:  DefaultSSLMode,
		password: DefaultPassword,
	}
	for _, opt := range opts {
		opt(client)
	}

	keyVals := []string{
		"user=" + escapeOptVal(client.username),
		"dbname=" + escapeOptVal(client.dbName),
		"sslmode=" + escapeOptVal(client.sslMode),
		"password=" + escapeOptVal(client.password),
	}

	db, err := sql.Open("postgres", strings.Join(keyVals, " "))
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %v", err)
	}

	if err := initDB(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize database: %v", err)
	}

	return &Client{
		db: db,
	}, nil
}

// ClientOption is used to pass options to NewClient.
type ClientOption func(c *Client)

// WithDBName returns a ClientOption that sets the database name for the
// connection string used to talk to Postgres.
//
// Usage:
//   NewClient(WithDBName("mydb"))
func WithDBName(n string) ClientOption {
	return func(c *Client) {
		c.dbName = n
	}
}

// WithUsername returns a CLientOption that sets the username for the
// Postgres connection string.
//
// Usage:
//   NewClient(WithUsername("myusername"))
func WithUsername(n string) ClientOption {
	return func(c *Client) {
		c.username = n
	}
}

// WithSSLMode sets the ssl mode in the connection string, e.g.,
//   NewClient(WithSSLMode("disabled"))
func WithSSLMode(mode string) ClientOption {
	return func(c *Client) {
		c.sslMode = mode
	}
}

// WithPassword sets the database password, e.g.,
//   NewClient(WithPassword(getPassword()))
func WithPassword(pwd string) ClientOption {
	return func(c *Client) {
		c.password = pwd
	}
}

// WithClaimant sets the claimant ID for this client, e.g.,
//   NewClient(WithClaimant(uuid.New()))
func WithClaimant(claimant uuid.UUID) ClientOption {
	return func(c *Client) {
		c.claimant = claimant
	}
}

// Close closes the underlying database connection.
func (c *Client) Close() error {
	return c.db.Close()
}

// ListQueues returns a slice of all queue names.
func (c *Client) ListQueues(ctx context.Context) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT DISTINCT queue FROM tasks")
	if err != nil {
		return nil, fmt.Errorf("failed to get queue names: %v", err)
	}
	defer rows.Close()
	var queues []string
	for rows.Next() {
		q := ""
		if err := rows.Scan(&q); err != nil {
			return nil, fmt.Errorf("queue scan failed: %v", err)
		}
		queues = append(queues, q)
	}
	if err := rows.Err; err != nil {
		return nil, fmt.Errorf("queue iteration failed: %v", err)
	}
	return queues, nil
}

// ListTasks returns a slice of all tasks in the given queue.
// TODO: Allow only expired, only owned, etc.
func (c *Client) ListTasks(ctx context.Context, q string) ([]*Task, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT id, version, queue, at, created, modified, claimant, value FROM tasks WHERE queue = $1", q)
	if err != nil {
		return nil, fmt.Errorf("failed to get tasks for queue %q: %v", q, err)
	}
	defer rows.Close()
	var tasks []*Task
	for rows.Next() {
		t := &Task{}
		if err := rows.Scan(&t.id, &t.version, &t.queue, &t.at, &t.created, &t.modified, &t.claimant, &t.value); err != nil {
			return nil, fmt.Errorf("task scan failed: %v", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err; err != nil {
		return nil, fmt.Errorf("task iteration failed for queue %q: %v", q, err)
	}
	return tasks, nil
}

// AddTask publishes a new task to the given queue, with value set to the specified bytes.
func (c *Client) AddTask(ctx context.Context, q string, val []byte) (string, error) {
	var id string
	if err := c.db.QueryRowContext(ctx, "INSERT INTO tasks (queue, value) VALUES ($1, $2) RETURNING id", q, val).Scan(&id); err != nil {
		return "", fmt.Errorf("bad task insertion: %v", err)
	}
	return id, nil
}

// ClaimTask attempts to get the next unclaimed task from the given queue. It
// blocks until one becomes available.
// TODO: specify AT
func (c *Client) ClaimTask(ctx context.Context, q string, args ...ClaimArg) (*Task, error) {
	const maxWait = time.Minute
	var curWait = time.Second
	for {
		task, err := c.maybeClaimTask(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		if task != nil {
			return task, nil
		}
		// No error, no task - we wait.
		select {
		case <-time.After(curWait):
			curWait *= 2
			if curWait > maxWait {
				curWait = maxWait
			}
		case <-ctx.Done():
			return nil, fmt.Errorf("context canceled for claim request in %q", q)
		}
	}
}

// claimArgs holds information used when claiming a task.
type claimArgs struct {
	until time.Time
}

// ClaimArg is used to modify how a claim happens, such as how long it should be claimed.
type ClaimArg func(a *claimArgs)

// Until sets the expire time of a claim.
func Until(t time.Time) ClaimArg {
	return func(a *claimArgs) {
		a.until = t
	}
}

// For sets the duration of a claim (expires d in the future).
func For(d time.Duration) ClaimArg {
	return func(a *claimArgs) {
		a.until = time.Now().Add(d)
	}
}

// maybeClaimTask attempts one time to claim a task from the given queue. If there are no tasks, it
// returns a nil error *and* a nil task. This allows the caller to decide whether to retry. Postgres
// doesn't really block until things become available, so this allows, e.g., an exponential backoff
// polling strategy to be used.
func (c *Client) maybeClaimTask(ctx context.Context, q string, args ...ClaimArg) (*Task, error) {
	task := new(Task)
	cargs := &claimArgs{}
	for _, arg := range args {
		arg(cargs)
	}
	if cargs.until.IsZero() {
		return nil, fmt.Errorf("no expiration time set for claim")
	}
	if err := c.db.QueryRowContext(ctx, `
		WITH top AS (
			SELECT * FROM tasks
			WHERE
				queue = 'group' AND
				at <= now()
			ORDER BY at, version, id ASC
			LIMIT 1
		)
		UPDATE tasks
		SET
			version=version+1,
			at = $1,
			claimant = $2,
			modified = now()
		WHERE id IN (SELECT id FROM top)
		RETURNING id, version, queue, at, created, modified, claimant, value;
	`, cargs.until, c.claimant).Scan(
		&task.id,
		&task.version,
		&task.queue,
		&task.at,
		&task.created,
		&task.modified,
		&task.claimant,
		&task.value,
	); err != nil {
		if err != sql.ErrNoRows {
			return nil, err
		}
		return nil, nil
	}
	return task, nil
}

// DeleteTask removes the task with the given ID and version from the task
// queue it happens to be in. If a task with exactly that ID and version is not
// present, an error is returned.
// TODO: Check AT, Claimant.
// TODO: use Modify as the main implementation.
func (c *Client) DeleteTask(ctx context.Context, id string, version int32) error {
	if _, err := c.db.ExecContext(ctx, "DELETE FROM tasks WHERE id = $1 and version = $2", id, version); err != nil {
		return fmt.Errorf("failed to delete task %s v%d: %v", id, version, err)
	}
	return nil
}

// ModifyResult holds results for a Modify operation.
type ModifyResult struct {
	// TODO: fill this in.
}

// Modify allows a batch modification operation to be done, gated on the
// existence of all task IDs and versions specified. Deletions, Updates, and
// Dependencies must be present. Insertions always work.
func (c *Client) Modify(ctx context.Context, modArgs ...ModifyArg) (*ModifyResult, error) {
	mod := new(modification)
	for _, arg := range modArgs {
		arg(mod)
	}
	// TODO: Now mod contains all proposed modifications.
	// We must craft a query that ensures that changes, deletes, and depends
	// all exist with the right versions, and then insert all inserts, delete
	// all deletes, and update all changes.
	return nil, nil
}

// modification contains all of the information for a single batch modification in the task store.
type modification struct {
	inserts []*taskData
	changes []*Task
	deletes []*taskID
	depends []*taskID
}

// ModifyArg is an argument to the Modify function, which does batch modifications to the task store.
type ModifyArg func(m *modification)

// InsertingInto creates an insert modification. Use like this:
//
//   cli.Modify(InsertingInto("my queue name", WithValue([]byte("hi there"))))
func InsertingInto(q string, insertArgs ...InsertArg) ModifyArg {
	data := &taskData{}
	for _, arg := range insertArgs {
		arg(data)
	}
	return func(m *modification) {
		m.inserts = append(m.inserts, data)
	}
}

// InsertArg is an argument to task insertion.
type InsertArg func(d *taskData)

// WithArrivalTime changes the arrival time to a fixed moment during task insertion.
func WithArrivalTime(at time.Time) InsertArg {
	return func(d *taskData) {
		d.at = at
	}
}

// WithArrivalTimeIn changes the arrival time by adding to time.Now(), e.g.,
//
//   cli.Modify(ctx,
//     InsertingInto("my queue",
//       WithTimeIn(2 * time.Minute)))
func WithArrivalTimeIn(duration time.Duration) InsertArg {
	return func(d *taskData) {
		d.at = time.Now().Add(duration)
	}
}

// WithValue sets the task's byte slice value during insertion.
//   cli.Modify(ctx,
//     InsertingInto("my queue",
//       WithValue([]byte("hi there"))))
func WithValue(value []byte) InsertArg {
	return func(d *taskData) {
		d.value = value
	}
}

// Deleting adds a deletion to a Modify call, e.g.,
//
//   cli.Modify(ctx, Deleting(id, version))
func Deleting(id string, version int32) ModifyArg {
	return func(m *modification) {
		m.deletes = append(m.deletes, &taskID{
			id:      id,
			version: version,
		})
	}
}

// DependingOn adds a dependency to a Modify call, e.g., to insert a task into
// "my queue" with data "hey", but only succeeding if a task with anotherID and
// someVersion exists:
//
//   cli.Modify(ctx,
//     InsertingInto("my queue",
//       WithValue([]byte("hey"))),
//       DependingOn(anotherID, someVersion))
func DependingOn(id string, version int32) ModifyArg {
	return func(m *modification) {
		m.depends = append(m.depends, &taskID{
			id:      id,
			version: version,
		})
	}
}

// Changing adds a task update to a Modify call, e.g., to modify
// the queue a task belongs in:
//
//   cli.Modify(ctx, Changing(myTask, QueueTo("a different queue name")))
func Changing(task *Task, changeArgs ...ChangeArg) ModifyArg {
	return func(m *modification) {
		newTask := *task
		for _, a := range changeArgs {
			a(&newTask)
		}
		m.changes = append(m.changes, &newTask)
	}
}

// ChangeArg is an argument to the Changing function used to create arguments
// for Modify, e.g., to change the queue and set the expiry time of a task to
// 5 minutes in the future, you would do something like this:
//
//   cli.Modify(ctx,
//     Changing(myTask,
//       QueueTo("a new queue"),
//	     ArrivalTimeBy(5 * time.Minute)))
type ChangeArg func(t *Task)

// QueueTo creates an option to modify a task's queue in the Changing function.
func QueueTo(q string) ChangeArg {
	return func(t *Task) {
		t.queue = q
	}
}

// ArrivalTimeTo sets a specific arrival time on a changed task in the Changing function.
func ArrivalTimeTo(at time.Time) ChangeArg {
	return func(t *Task) {
		t.at = at
	}
}

// ArrivalTimeBy sets the arrival time to a time in the future, by the given duration.
func ArrivalTimeBy(d time.Duration) ChangeArg {
	return func(t *Task) {
		t.at = t.at.Add(d)
	}
}

// ValueTo sets the changing task's value to the given byte slice.
func ValueTo(v []byte) ChangeArg {
	return func(t *Task) {
		t.value = v
	}
}
