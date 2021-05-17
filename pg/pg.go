// Package pg provides an entroq.Backend using PostgreSQL. Use Opener with
// entroq.New to create a task client that talks to a PostgreSQL backend.
package pg // import "entrogo.com/entroq/pg"

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/url"
	"strings"
	"time"

	"entrogo.com/entroq"
	"entrogo.com/entroq/subq"
	"github.com/google/uuid"
	"github.com/lib/pq"
	pkgerrors "github.com/pkg/errors"
)

func escp(p string) string {
	return "'" + strings.NewReplacer("\\", "\\\\", "'", "\\'").Replace(p) + "'"
}

// SSLMode is used to request a particular PostgreSQL SSL mode.
type SSLMode string

const (
	SSLDisable    SSLMode = "disable"     // Always non-SSL.
	SSLAllow      SSLMode = "allow"       // Try non-SSL first, fall back to SSL.
	SSLPrefer     SSLMode = "prefer"      // Try SSL first, fall back to non-SSL.
	SSLRequire    SSLMode = "require"     // Only try SSL.
	SSLVerifyCA   SSLMode = "verify-ca"   // Only SSL, check server against CA.
	SSLVerifyFull SSLMode = "verify-full" // Only SSL, check CA and host name.
)

type pgOptions struct {
	db       string
	user     string
	password string

	attempts int
	nw       entroq.NotifyWaiter

	sslMode SSLMode

	sslClientKeyFile  string
	sslClientCertFile string
	sslServerCAFile   string
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

// WithSSL provides SSL-specific options to the database connection.
func WithSSL(mode SSLMode, sslOpts ...PGOpt) PGOpt {
	return func(opts *pgOptions) {
		opts.sslMode = mode
		for _, o := range sslOpts {
			o(opts)
		}
	}
}

// WithSSLClientFiles specfies the client cert and key files for the connection.
func WithSSLClientFiles(certFile, keyFile string) PGOpt {
	return func(opts *pgOptions) {
		opts.sslClientCertFile = certFile
		opts.sslClientKeyFile = keyFile
	}
}

// WithSSLServerCAFile specifies the CA file for verifying the server.
func WithSSLServerCAFile(caFile string) PGOpt {
	return func(opts *pgOptions) {
		opts.sslServerCAFile = caFile
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

		sslMode: SSLDisable,
	}
	for _, o := range opts {
		o(options)
	}

	return func(ctx context.Context) (entroq.Backend, error) {
		u, err := url.Parse("postgres://" + hostPort)
		if err != nil {
			return nil, pkgerrors.Wrapf(err, "failed to parse hostport %q", hostPort)
		}
		host := u.Hostname()
		port := u.Port()

		if port != "" && host == "" {
			host = "::"
		}

		if host != "" && port == "" {
			port = "5432"
		}

		params := []string{
			"sslmode=" + string(options.sslMode),
		}

		if options.user != "" {
			params = append(params, fmt.Sprintf("user=%s", escp(options.user)))
		}

		if options.password != "" {
			params = append(params, fmt.Sprintf("password=%s", escp(options.password)))
		}

		if host != "" {
			params = append(params, fmt.Sprintf("host=%s", escp(host)))
		}

		if port != "" {
			params = append(params, fmt.Sprintf("port=%s", port))
		}

		if options.sslClientKeyFile != "" {
			params = append(params, "sslkey="+url.QueryEscape(options.sslClientKeyFile))
		}

		if options.sslClientCertFile != "" {
			params = append(params, "sslcert="+url.QueryEscape(options.sslClientCertFile))
		}

		if options.sslServerCAFile != "" {
			params = append(params, "sslrootcert="+url.QueryEscape(options.sslServerCAFile))
		}

		connStr := strings.Join(params, " ")

		db, err := sql.Open("postgres", connStr)
		if err != nil {
			return nil, pkgerrors.Wrap(err, "failed to open postgres DB")
		}
		for i := 0; i < options.attempts; i++ {
			if err = db.PingContext(ctx); err == nil {
				return New(ctx, db, options.nw)
			}
			if i < options.attempts-1 {
				select {
				case <-ctx.Done():
					return nil, pkgerrors.Wrap(ctx.Err(), "pg opener")
				case <-time.After(5 * time.Second):
				}
			}
		}
		return nil, pkgerrors.Wrap(err, "time out postgres init")
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
		return nil, pkgerrors.Wrap(err, "failed to initialize database")
	}
	return b, nil
}

// Close closes the underlying database connection.
func (b *backend) Close() error {
	return pkgerrors.Wrap(b.db.Close(), "pg backend close")
}

// initDB sets up the database to have the appropriate tables and necessary
// extensions to work as a task queue backend.
func (b *backend) initDB(ctx context.Context) error {
	// Note: ALTER TABLE commands are given to ensure that older versions of EntroQ databases are automatically upgraded.
	// EntroQ systems were in service before Claims, Attempt, or Err were added to task types, so we attempt to add these
	// columns to an existing table, then we create the whole thing if it isn't there.
	_, err := b.db.ExecContext(ctx, `
		ALTER TABLE IF EXISTS tasks ADD COLUMN IF NOT EXISTS claims INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE IF EXISTS tasks ADD COLUMN IF NOT EXISTS attempt INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE IF EXISTS tasks ADD COLUMN IF NOT EXISTS err TEXT NOT NULL DEFAULT '';
		CREATE TABLE IF NOT EXISTS tasks (
		  id UUID PRIMARY KEY NOT NULL,
		  version INTEGER NOT NULL DEFAULT 0,
		  queue TEXT NOT NULL DEFAULT '',
		  at TIMESTAMP WITH TIME ZONE NOT NULL,
		  created TIMESTAMP WITH TIME ZONE,
		  modified TIMESTAMP WITH TIME ZONE NOT NULL,
		  claimant UUID,
		  value BYTEA,
		  claims INTEGER NOT NULL DEFAULT 0,
		  attempt INTEGER NOT NULL DEFAULT 0,
		  err TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS byID ON tasks (id);
		CREATE INDEX IF NOT EXISTS byVersion ON tasks (version);
		CREATE INDEX IF NOT EXISTS byQueue ON tasks (queue);
		CREATE INDEX IF NOT EXISTS byQueueAt ON tasks (queue, at);
	`)
	return pkgerrors.Wrap(err, "initDB")
}

// Queues returns the queues and their sizes.
func (b *backend) Queues(ctx context.Context, qq *entroq.QueuesQuery) (map[string]int, error) {
	return entroq.QueuesFromStats(b.QueueStats(ctx, qq))
}

// QueueStats returns a mapping from queue names to their statistics.
func (b *backend) QueueStats(ctx context.Context, qq *entroq.QueuesQuery) (map[string]*entroq.QueueStat, error) {
	q := `SELECT
			queue,
			COUNT(*) AS count,
			COUNT(*) FILTER(WHERE at > NOW() AND claimant != '00000000-0000-0000-0000-000000000000') AS claimed,
			COUNT(*) FILTER(WHERE at <= NOW()) as available,
			MAX(claims) AS max_claims
		FROM tasks`
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
		return nil, pkgerrors.Wrap(err, "queue names")
	}

	defer rows.Close()
	queues := make(map[string]*entroq.QueueStat)
	for rows.Next() {
		var (
			q         string
			count     int
			claimed   int
			available int
			maxClaims int
		)
		if err := rows.Scan(&q, &count, &claimed, &available, &maxClaims); err != nil {
			return nil, pkgerrors.Wrap(err, "row scan")
		}
		queues[q] = &entroq.QueueStat{
			Name:      q,
			Size:      count,
			Claimed:   claimed,
			Available: available,
			MaxClaims: maxClaims,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, pkgerrors.Wrap(err, "queue iteration")
	}
	return queues, nil
}

// Tasks returns a slice of all tasks in the given queue.
func (b *backend) Tasks(ctx context.Context, tq *entroq.TasksQuery) ([]*entroq.Task, error) {
	q := "SELECT id, version, queue, at, created, modified, claimant, value, claims, attempt, err FROM tasks WHERE true"
	var values []interface{}

	if tq.Queue != "" {
		q += fmt.Sprintf(" AND queue = $%d", len(values)+1)
		values = append(values, tq.Queue)
	}

	if tq.Claimant != uuid.Nil {
		q += fmt.Sprintf(" AND (claimant = $%d OR claimant = $%d OR at < NOW())", len(values)+1, len(values)+2)
		values = append(values, uuid.Nil.String(), tq.Claimant)
	}

	// Add IDs if a set of limiting IDs has been requested.
	strIDs := make([]string, 0, len(tq.IDs))
	for _, id := range tq.IDs {
		strIDs = append(strIDs, id.String())
	}
	if len(strIDs) != 0 {
		q += fmt.Sprintf(" AND id = any($%d)", len(values)+1)
		values = append(values, pq.StringArray(strIDs))
	}

	if tq.Limit > 0 {
		// Safe to directly append, since it's an int.
		q += fmt.Sprintf(" LIMIT %d", tq.Limit)
	}

	rows, err := b.db.QueryContext(ctx, q, values...)
	if err != nil {
		return nil, pkgerrors.Wrapf(err, "queue tasks %q", tq.Queue)
	}
	defer rows.Close()
	var tasks []*entroq.Task
	for rows.Next() {
		t := &entroq.Task{}
		if err := rows.Scan(&t.ID, &t.Version, &t.Queue, &t.At, &t.Created, &t.Modified, &t.Claimant, &t.Value, &t.Claims, &t.Attempt, &t.Err); err != nil {
			return nil, pkgerrors.Wrap(err, "task scan")
		}
		// NOTE: we can make this more efficient by not even asking for the
		// value, but it complicates the code a lot and may not be worth the
		// maintainability hit.
		if tq.OmitValues {
			t.Value = nil
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, pkgerrors.Wrapf(err, "queue task iteration %q", tq.Queue)
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

// TryClaim attempts to claim an "arrived" task from any of the specified
// queues, attempting to do so fairly across queues. Returns an error if
// something goes wrong, a nil task if there is nothing to claim.
func (b *backend) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	// Note that we loop here instead of just using `= any(queues)` in the
	// WHERE clause because that cannot guarantee inter-queue fairness. And,
	// DISTINCT ON doesn't work in FOR UPDATE, etc.
	//
	// This is just as good, though, because we poll them all fairly quickly, and it does
	// not reduce correctness.
	qs := append(make([]string, 0, len(cq.Queues)), cq.Queues...)
	rand.Shuffle(len(qs), func(i, j int) { qs[i], qs[j] = qs[j], qs[i] })

	for _, q := range qs {
		t, err := b.tryClaimOne(ctx, q, cq)
		if err != nil {
			return nil, pkgerrors.Wrap(err, "try claim")
		}
		if t != nil {
			return t, nil
		}
	}
	return nil, nil
}

// tryClaimOne attempts to claim an "arrived" task from the specified queue.
// Returns an error if something goes wrong, a nil task if there is nothing to
// claim.
func (b *backend) tryClaimOne(ctx context.Context, q string, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	task := new(entroq.Task)
	if cq.Duration == 0 {
		return nil, pkgerrors.Errorf("no duration set for claim %q", cq.Queues)
	}
	now := time.Now()

	// Use of SKIP LOCKED:
	// - https://dba.stackexchange.com/questions/69471/postgres-update-limit-1
	// - https://blog.2ndquadrant.com/what-is-select-skip-locked-for-in-postgresql-9-5/
	if err := b.db.QueryRowContext(ctx, `
		UPDATE tasks
		SET
			version = version + 1,
			claims = claims + 1,
			at = $1,
			claimant = $2,
			modified = $3
		WHERE
			(id, version) IN (
				SELECT id, version
				FROM tasks
				WHERE
					queue = $4 AND
					at < $5
				ORDER BY random()
				FOR UPDATE SKIP LOCKED
				LIMIT 1
			)
		RETURNING id, version, queue, at, created, modified, claimant, value, claims, attempt, err
	`, now.Add(cq.Duration), cq.Claimant, now, q, now).Scan(
		&task.ID,
		&task.Version,
		&task.Queue,
		&task.At,
		&task.Created,
		&task.Modified,
		&task.Claimant,
		&task.Value,
		&task.Claims,
		&task.Attempt,
		&task.Err,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, pkgerrors.Wrap(err, "claim update")
	}

	return task, nil
}

func isSerialization(err error) bool {
	if err == nil {
		return false
	}
	e, ok := pkgerrors.Cause(err).(*pq.Error)
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
			return nil, nil, pkgerrors.Wrap(err, "pg modify dependency")
		}
		if !isSerialization(err) {
			// We didn't get a retryable serialization error, return.
			return nil, nil, pkgerrors.Wrap(err, "pg modify unknown")
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
	tx, err := b.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, nil, pkgerrors.Wrap(err, "failed to start transaction")
	}
	defer func() {
		if err != nil {
			if rerr := tx.Rollback(); rerr != nil {
				log.Printf("Error rolling back: %v", rerr)
			}
		} else {
			err = tx.Commit()
		}
	}()

	foundDeps, err := depQuery(ctx, tx, mod)
	if err != nil {
		return nil, nil, pkgerrors.Wrap(err, "get deps")
	}

	if err := mod.DependencyError(foundDeps); err != nil {
		return nil, nil, pkgerrors.Wrap(err, "dependency issue found")
	}

	// Once we get here, we know that all of the dependencies were present.
	// These should now all succeed.

	for _, tid := range mod.Deletes {
		q := "DELETE FROM tasks WHERE id = $1 AND version = $2"
		if _, err := tx.ExecContext(ctx, q, tid.ID, tid.Version); err != nil {
			return nil, nil, pkgerrors.Wrapf(err, "pg modify delete %s", tid)
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
			INSERT INTO tasks (id, version, queue, at, claimant, value, created, modified, attempt, err)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING id, version, queue, at, claimant, value, created, modified, attempt, err
		`, id, 0, ins.Queue, at, mod.Claimant, ins.Value, now, now, ins.Attempt, ins.Err)
		if err := row.Scan(&t.ID, &t.Version, &t.Queue, &t.At, &t.Claimant, &t.Value, &t.Created, &t.Modified, &t.Attempt, &t.Err); err != nil {
			return nil, nil, pkgerrors.Wrap(err, "pg modify insert scan")
		}
		inserted = append(inserted, t)
	}

	for _, chg := range mod.Changes {
		t := new(entroq.Task)
		row := tx.QueryRowContext(ctx, `
			UPDATE tasks SET version = version + 1, modified = $1, queue = $2, at = $3, value = $4, attempt = $5, err = $6
			WHERE id = $7 AND version = $8
			RETURNING id, version, queue, at, claimant, modified, created, value, attempt, err
		`, now, chg.Queue, chg.At, chg.Value, chg.Attempt, chg.Err, chg.ID, chg.Version)
		if err := row.Scan(&t.ID, &t.Version, &t.Queue, &t.At, &t.Claimant, &t.Modified, &t.Created, &t.Value, &t.Attempt, &t.Err); err != nil {
			return nil, nil, pkgerrors.Wrapf(err, "pg modify update scan %s", chg.ID)
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
		return nil, pkgerrors.Wrap(err, "pg dep query")
	}

	foundDeps := make(map[uuid.UUID]*entroq.Task)

	if len(dependencies) == 0 {
		return foundDeps, nil
	}

	// Form a SELECT FOR UPDATE that looks at all of these rows so that we can
	// touch them all while inserting, deleting, and changing, and so we can
	// guarantee that dependencies don't disappear while we're at it (we need
	// to know that the dependencies are there while we work).
	strIDs := make([]string, 0, len(dependencies))
	for id := range dependencies {
		strIDs = append(strIDs, id.String())
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, version, queue, at, claimant, value, created, modified, attempt, err FROM tasks
		WHERE id = any($1) FOR UPDATE
	`, pq.Array(strIDs))
	if err != nil {
		return nil, pkgerrors.Wrap(err, "pg dep query")
	}
	defer rows.Close()
	for rows.Next() {
		t := new(entroq.Task)
		if err := rows.Scan(&t.ID, &t.Version, &t.Queue, &t.At, &t.Claimant, &t.Value, &t.Created, &t.Modified, &t.Attempt, &t.Err); err != nil {
			return nil, pkgerrors.Wrap(err, "pg dep scan")
		}
		if foundDeps[t.ID] != nil {
			return nil, pkgerrors.Errorf("pg duplicate ID %q found", t.ID)
		}
		foundDeps[t.ID] = t
	}
	if err := rows.Err(); err != nil {
		return nil, pkgerrors.Wrap(err, "pg dep iteration")
	}

	return foundDeps, nil
}

// Time returns the time used in all calculations in this process.
func (b *backend) Time(ctx context.Context) (time.Time, error) {
	row := b.db.QueryRowContext(ctx, "SELECT now()")
	var t time.Time
	if err := row.Scan(&t); err != nil {
		return time.Time{}, pkgerrors.Wrap(err, "postgres time")
	}
	return t, nil
}
