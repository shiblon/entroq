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
func initDB(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
		CREATE TABLE IF NOT EXISTS tasks (
		  id UUID PRIMARY KEY NOT NULL DEFAULT UUID_GENERATE_V4(),
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

// TaskID contains the identifying parts of a task. If IDs don't match
// (identifier and version together), then operations fail on those tasks.
type TaskID struct {
	ID      uuid.UUID
	Version int32
}

func (t TaskID) String() string {
	return fmt.Sprintf("%s:v%d", t.ID, t.Version)
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

	id      uuid.UUID
	version int32

	at       time.Time
	claimant uuid.UUID
	value    []byte

	created  time.Time
	modified time.Time
}

// String returns a string representation of this task.
func (t *Task) String() string {
	return "Task: " + strings.Join([]string{
		fmt.Sprintf("[%q %s v%d]", t.queue, t.id, t.version),
		fmt.Sprintf("[at=%v claimant=%s val=%s]", t.at, t.claimant, string(t.value)),
		fmt.Sprintf("[c=%v m=%v]", t.created, t.modified),
	}, " ")
}

// Queue returns the queue name for this task.
func (t *Task) Queue() string { return t.queue }

// ID returns the ID of this task (not including the version, which is also part of its key).
func (t *Task) ID() uuid.UUID { return t.id }

// Version returns the version of this task (unique when paired with ID).
func (t *Task) Version() int32 { return t.version }

// ArrivalTime returns the arrival time of this task.
func (t *Task) ArrivalTime() time.Time { return t.at }

// Claimant returns the ID of the owner of this task, if not expired.
func (t *Task) Claimant() uuid.UUID { return t.claimant }

// Value returns the value of this task.
func (t *Task) Value() []byte { return t.value }

// Created returns the creation time of this task.
func (t *Task) Created() time.Time { return t.created }

// Modified returns the modification time of this task.
func (t *Task) Modified() time.Time { return t.modified }

// AsDeletion returns a CommitArg that can be used in the Commit function, e.g.,
//
//   cli.Commit(ctx, task1.AsDeletion())
//
// The above would cause the given task to be deleted, if it can be. It is
// shorthand for
//
//   cli.Commit(ctx, Deleting(task1.ID(), task1.Version()))
func (t *Task) AsDeletion() CommitArg {
	return Deleting(t.id, t.version)
}

// AsChange returns a CommitArg that can be used in the Commit function, e.g.,
//
//   cli.Commit(ctx, task1.AsChange(ArrivalTimeBy(2 * time.Minute)))
//
// The above is shorthand for
//
//   cli.Commit(ctx, Changing(task1, ArrivalTimeBy(2 * time.Minute)))
func (t *Task) AsChange(args ...ChangeArg) CommitArg {
	return Changing(t, args...)
}

// AsDependency returns a CommitArg that can be used to create a Commit dependency, e.g.,
//
//   cli.Commit(ctx, task.AsDependency())
//
// That is shorthand for
//
//   cli.Commit(ctx, DependingOn(task.ID(), task.Version()))
func (t *Task) AsDependency() CommitArg {
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
func NewClient(ctx context.Context, opts ...ClientOption) (*Client, error) {
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
	client.db = db

	if err := initDB(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize database: %v", err)
	}

	return client, nil
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

// Queues returns a slice of all queue names.
func (c *Client) Queues(ctx context.Context) ([]string, error) {
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue iteration failed: %v", err)
	}
	return queues, nil
}

// Tasks returns a slice of all tasks in the given queue.
// TODO: Allow only expired, only owned, etc.
func (c *Client) Tasks(ctx context.Context, q string) ([]*Task, error) {
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("task iteration failed for queue %q: %v", q, err)
	}
	return tasks, nil
}

// Claim attempts to get the next unclaimed task from the given queue. It
// blocks until one becomes available.
func (c *Client) Claim(ctx context.Context, q string, args ...ClaimArg) (*Task, error) {
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
	now   time.Time
	until time.Time
}

func newClaimArgs() *claimArgs {
	return &claimArgs{now: time.Now()}
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
		a.until = a.now.Add(d)
	}
}

// maybeClaimTask attempts one time to claim a task from the given queue. If there are no tasks, it
// returns a nil error *and* a nil task. This allows the caller to decide whether to retry. Postgres
// doesn't really block until things become available, so this allows, e.g., an exponential backoff
// polling strategy to be used.
func (c *Client) maybeClaimTask(ctx context.Context, q string, args ...ClaimArg) (*Task, error) {
	task := new(Task)
	cargs := newClaimArgs()
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
				queue = $1 AND
				at <= NOW()
			ORDER BY at, version, id ASC
			LIMIT 1
		)
		UPDATE tasks
		SET
			version=version+1,
			at = $2,
			claimant = $3,
			modified = NOW()
		WHERE id IN (SELECT id FROM top)
		RETURNING id, version, queue, at, created, modified, claimant, value
	`, q, cargs.until, c.claimant).Scan(
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

// DependencyErr is returned when a dependency is missing when modifying the task store.
type DependencyErr struct {
	Depends []*TaskID
	Deletes []*TaskID
	Changes []*TaskID

	Claims []*TaskID
}

// HasMissing indicates whether there was anything missing in this error.
func (m DependencyErr) HasMissing() bool {
	return len(m.Depends) > 0 || len(m.Deletes) > 0 || len(m.Changes) > 0
}

// HasClaims indicates whether any of the tasks were claimed by another claimant and unexpired.
func (m DependencyErr) HasClaims() bool {
	return len(m.Claims) > 0
}

// Error produces a helpful error string indicating what was missing.
func (m DependencyErr) Error() string {
	lines := []string{
		"DependencyErr:",
	}
	if len(m.Depends) > 0 {
		lines = append(lines, "\tmissing depends:")
		for _, tid := range m.Depends {
			lines = append(lines, fmt.Sprintf("\t\t%s", tid))
		}
	}
	if len(m.Deletes) > 0 {
		lines = append(lines, "\tmissing deletes:")
		for _, tid := range m.Deletes {
			lines = append(lines, fmt.Sprintf("\t\t%s", tid))
		}
	}
	if len(m.Changes) > 0 {
		lines = append(lines, "\tmissing changes:")
		for _, tid := range m.Changes {
			lines = append(lines, fmt.Sprintf("\t\t%s", tid))
		}
	}
	if len(m.Claims) > 0 {
		lines = append(lines, "\tclaimed modified tasks:")
		for _, tid := range m.Claims {
			lines = append(lines, fmt.Sprintf("\t\t%s", tid))
		}
	}
	return strings.Join(lines, "\n")
}

// IsDependency indicates whether the given error is a dependency error.
func IsDependency(err error) bool {
	_, ok := err.(DependencyErr)
	return ok
}

// Commit allows a batch modification operation to be done, gated on the
// existence of all task IDs and versions specified. Deletions, Updates, and
// Dependencies must be present. The transaction all fails or all succeeds.
//
// Returns all inserted task IDs, and an error if it could not proceed. If the error
// was due to missing dependencies, a *DependencyErr is returned, which can be checked for
// by calling IsDependency(err).
func (c *Client) Commit(ctx context.Context, modArgs ...CommitArg) (inserted []uuid.UUID, changed []*Task, err error) {
	mod := newModification(c.claimant)
	for _, arg := range modArgs {
		arg(mod)
	}

	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
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

	missingChanges, claimedChanges := mod.badChanges(foundDeps)
	missingDeletes, claimedDeletes := mod.badDeletes(foundDeps)
	missingDepends := mod.missingDepends(foundDeps)
	if len(missingChanges) > 0 || len(claimedChanges) > 0 || len(missingDeletes) > 0 || len(claimedDeletes) > 0 || len(missingDepends) > 0 {
		return nil, nil, DependencyErr{
			Changes: missingChanges,
			Deletes: missingDeletes,
			Depends: mod.missingDepends(foundDeps),
			Claims:  append(claimedDeletes, claimedChanges...),
		}
	}

	// Once we get here, we know that all of the dependencies were present.
	// These should now all succeed.

	for _, td := range mod.inserts {
		columns := []string{"queue", "claimant", "value"}
		values := []interface{}{td.queue, c.claimant, td.value}

		if !td.at.IsZero() {
			columns = append(columns, "at")
			values = append(values, td.at)
		}

		var placeholders []string
		for i := range columns {
			placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
		}

		q := "INSERT INTO tasks (" + strings.Join(columns, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ") RETURNING id"
		var id uuid.UUID
		if err := tx.QueryRowContext(ctx, q, values...).Scan(&id); err != nil {
			return nil, nil, fmt.Errorf("insert failed in queue %q: %v", td.queue, err)
		}
		inserted = append(inserted, id)
	}

	for _, tid := range mod.deletes {
		q := "DELETE FROM tasks WHERE id = $1 AND version = $2"
		if _, err := tx.ExecContext(ctx, q, tid.ID, tid.Version); err != nil {
			return nil, nil, fmt.Errorf("delete failed for task %q: %v", tid.ID, err)
		}
	}

	for _, t := range mod.changes {
		q := `UPDATE tasks SET
				version = version + 1,
				modified = NOW(),
				queue = $1,
				at = $2,
				value = $3
			WHERE id = $4 AND version = $5
			RETURNING id, version, queue, at, claimant, modified, created, value`
		nt := &Task{}
		row := tx.QueryRowContext(ctx, q, t.queue, t.at, t.value, t.id, t.version)
		if err := row.Scan(&nt.id, &nt.version, &nt.queue, &nt.at, &nt.claimant, &nt.modified, &nt.created, &nt.value); err != nil {
			return nil, nil, fmt.Errorf("scan failed for newly-changed task %q: %v", t.id, err)
		}
		changed = append(changed, nt)
	}

	return inserted, changed, nil
}

// depQuery sends a query to the database, within a transaction, that will lock
// all rows for all dependencies of a modification, allowing updates to be
// performed on those rows without other transactions getting in the middle of
// things. Returns a map from ID to version for every dependency that is found,
// or an error if the query itself fails.
func depQuery(ctx context.Context, tx *sql.Tx, m *modification) (map[uuid.UUID]*Task, error) {
	// We must craft a query that ensures that changes, deletes, and depends
	// all exist with the right versions, and then insert all inserts, delete
	// all deletes, and update all changes.
	dependencies, err := m.allDependencies()
	if err != nil {
		return nil, err
	}

	foundDeps := make(map[uuid.UUID]*Task)

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
		t := new(Task)
		if err := rows.Scan(&t.id, &t.version, &t.queue, &t.at, &t.claimant, &t.value, &t.created, &t.modified); err != nil {
			return nil, fmt.Errorf("row scan failed: %v", err)
		}
		if foundDeps[t.id] != nil {
			return nil, fmt.Errorf("duplicate ID %q found in database (more than one version)", t.id)
		}
		foundDeps[t.id] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error scanning dependencies: %v", err)
	}

	return foundDeps, nil
}

// modification contains all of the information for a single batch modification in the task store.
type modification struct {
	claimant uuid.UUID
	now      time.Time

	inserts []*taskData
	changes []*Task
	deletes []*TaskID
	depends []*TaskID
}

func newModification(claimant uuid.UUID) *modification {
	return &modification{claimant: claimant, now: time.Now()}
}

// modDependencies returns a dependency map for all modified dependencies
// (deletes and changes).
func (m *modification) modDependencies() (map[uuid.UUID]int32, error) {
	deps := make(map[uuid.UUID]int32)
	for _, t := range m.changes {
		if _, ok := deps[t.id]; ok {
			return nil, fmt.Errorf("duplicates found in dependencies")
		}
		deps[t.id] = t.version
	}
	for _, t := range m.deletes {
		if _, ok := deps[t.ID]; ok {
			return nil, fmt.Errorf("duplicates found in dependencies")
		}
		deps[t.ID] = t.Version
	}
	return deps, nil
}

// allDependencies returns a dependency map from ID to version, and returns an
// error if there are duplicates. Changes, Deletions, and Dependencies should
// be disjoint sets.
func (m *modification) allDependencies() (map[uuid.UUID]int32, error) {
	deps, err := m.modDependencies()
	if err != nil {
		return nil, err
	}
	for _, t := range m.depends {
		if _, ok := deps[t.ID]; ok {
			return nil, fmt.Errorf("duplicates found in dependencies")
		}
		deps[t.ID] = t.Version
	}
	return deps, nil
}

func (m *modification) otherwiseClaimed(task *Task) bool {
	var zeroUUID uuid.UUID
	return task.at.After(m.now) && task.claimant != zeroUUID && task.claimant != m.claimant
}

func (m *modification) badChanges(foundDeps map[uuid.UUID]*Task) (missing []*TaskID, claimed []*TaskID) {
	for _, t := range m.changes {
		found := foundDeps[t.id]
		if found == nil {
			missing = append(missing, &TaskID{
				ID:      t.id,
				Version: t.version,
			})
		}
		if m.otherwiseClaimed(found) {
			claimed = append(claimed, &TaskID{
				ID:      t.id,
				Version: t.version,
			})
		}
	}
	return missing, claimed
}

func (m *modification) badDeletes(foundDeps map[uuid.UUID]*Task) (missing []*TaskID, claimed []*TaskID) {
	for _, t := range m.deletes {
		found := foundDeps[t.ID]
		if found == nil {
			missing = append(missing, t)
		}
		if m.otherwiseClaimed(found) {
			claimed = append(claimed, t)
		}
	}
	return missing, claimed
}

func (m *modification) missingDepends(foundDeps map[uuid.UUID]*Task) []*TaskID {
	var missing []*TaskID
	for _, t := range m.depends {
		if _, ok := foundDeps[t.ID]; !ok {
			missing = append(missing, t)
		}
	}
	return missing
}

// CommitArg is an argument to the Commit function, which does batch modifications to the task store.
type CommitArg func(m *modification)

// InsertingInto creates an insert modification. Use like this:
//
//   cli.Commit(InsertingInto("my queue name", WithValue([]byte("hi there"))))
func InsertingInto(q string, insertArgs ...InsertArg) CommitArg {
	return func(m *modification) {
		data := &taskData{queue: q}
		for _, arg := range insertArgs {
			arg(m.now, data)
		}
		m.inserts = append(m.inserts, data)
	}
}

// InsertArg is an argument to task insertion.
type InsertArg func(now time.Time, d *taskData)

// WithArrivalTime changes the arrival time to a fixed moment during task insertion.
func WithArrivalTime(at time.Time) InsertArg {
	return func(now time.Time, d *taskData) {
		d.at = at
	}
}

// WithArrivalTimeIn computes the arrival time based on the duration from now, e.g.,
//
//   cli.Commit(ctx,
//     InsertingInto("my queue",
//       WithTimeIn(2 * time.Minute)))
func WithArrivalTimeIn(duration time.Duration) InsertArg {
	return func(now time.Time, d *taskData) {
		d.at = now.Add(duration)
	}
}

// WithValue sets the task's byte slice value during insertion.
//   cli.Commit(ctx,
//     InsertingInto("my queue",
//       WithValue([]byte("hi there"))))
func WithValue(value []byte) InsertArg {
	return func(now time.Time, d *taskData) {
		d.value = value
	}
}

// Deleting adds a deletion to a Commit call, e.g.,
//
//   cli.Commit(ctx, Deleting(id, version))
func Deleting(id uuid.UUID, version int32) CommitArg {
	return func(m *modification) {
		m.deletes = append(m.deletes, &TaskID{
			ID:      id,
			Version: version,
		})
	}
}

// DependingOn adds a dependency to a Commit call, e.g., to insert a task into
// "my queue" with data "hey", but only succeeding if a task with anotherID and
// someVersion exists:
//
//   cli.Commit(ctx,
//     InsertingInto("my queue",
//       WithValue([]byte("hey"))),
//       DependingOn(anotherID, someVersion))
func DependingOn(id uuid.UUID, version int32) CommitArg {
	return func(m *modification) {
		m.depends = append(m.depends, &TaskID{
			ID:      id,
			Version: version,
		})
	}
}

// Changing adds a task update to a Commit call, e.g., to modify
// the queue a task belongs in:
//
//   cli.Commit(ctx, Changing(myTask, QueueTo("a different queue name")))
func Changing(task *Task, changeArgs ...ChangeArg) CommitArg {
	return func(m *modification) {
		newTask := *task
		for _, a := range changeArgs {
			a(&newTask)
		}
		m.changes = append(m.changes, &newTask)
	}
}

// ChangeArg is an argument to the Changing function used to create arguments
// for Commit, e.g., to change the queue and set the expiry time of a task to
// 5 minutes in the future, you would do something like this:
//
//   cli.Commit(ctx,
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
