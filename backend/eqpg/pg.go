// Package eqpg provides an entroq.Backend using PostgreSQL. Use Opener with
// entroq.New to create a task client that talks to a PostgreSQL backend.
package eqpg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/subq"
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
			return nil, fmt.Errorf("failed to parse hostport %q: %w", hostPort, err)
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
			"database=" + escp(options.db),
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
			return nil, fmt.Errorf("failed to open postgres DB: %w", err)
		}
		for i := 0; i < options.attempts; i++ {
			if err = db.PingContext(ctx); err == nil {
				return New(ctx, db, options.nw)
			}
			if i < options.attempts-1 {
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("pg opener: %w", ctx.Err())
				case <-time.After(5 * time.Second):
				}
			}
		}
		return nil, fmt.Errorf("time out postgres init: %w", err)
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
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}
	return b, nil
}

// Close closes the underlying database connection.
func (b *backend) Close() error {
	if err := b.db.Close(); err != nil {
		return fmt.Errorf("pg backend close: %w", err)
	}
	return nil
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
		return nil, fmt.Errorf("queue names: %w", err)
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
			return nil, fmt.Errorf("row scan: %w", err)
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
		return nil, fmt.Errorf("queue iteration: %w", err)
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
		return nil, fmt.Errorf("queue tasks %q: %w", tq.Queue, err)
	}
	defer rows.Close()
	var tasks []*entroq.Task
	for rows.Next() {
		t := &entroq.Task{}
		if err := rows.Scan(&t.ID, &t.Version, &t.Queue, &t.At, &t.Created, &t.Modified, &t.Claimant, &t.Value, &t.Claims, &t.Attempt, &t.Err); err != nil {
			return nil, fmt.Errorf("task scan: %w", err)
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
		return nil, fmt.Errorf("queue task iteration %q: %w", tq.Queue, err)
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
			return nil, fmt.Errorf("try claim: %w", err)
		}
		if t != nil {
			return t, nil
		}
	}
	return nil, nil
}

// tryClaimOne attempts to claim an "arrived" task from the specified queue.
// Returns an error if something goes wrong, a nil task if there is nothing to
// claim. Delegates all bucket selection and fallback logic to the
// entroq_try_claim_one stored procedure.
func (b *backend) tryClaimOne(ctx context.Context, q string, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	if cq.Duration == 0 {
		return nil, fmt.Errorf("no duration set for claim %q", cq.Queues)
	}
	task := new(entroq.Task)
	err := b.db.QueryRowContext(ctx,
		`SELECT id, version, queue, at, created, modified, claimant, value, claims, attempt, err
		 FROM entroq_try_claim_one($1, $2, $3)`,
		q, cq.Claimant, fmt.Sprintf("%d microseconds", cq.Duration/time.Microsecond),
	).Scan(
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
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("try claim one: %w", err)
	}
	return task, nil
}

// isRetryable returns true for PostgreSQL errors that indicate a transaction
// should be retried: serialization failures (40001) and deadlocks (40P01).
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if pgerr := new(pq.Error); errors.As(err, &pgerr) {
		return pgerr.Code == "40001" || pgerr.Code == "40P01"
	}
	return false
}

// Modify attempts to apply an atomic modification to the task store. Either
// all succeeds or all fails.
func (b *backend) Modify(ctx context.Context, mod *entroq.Modification) (inserted, changed []*entroq.Task, err error) {
	const minBackoff = 10 * time.Millisecond
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
			return nil, nil, fmt.Errorf("pg modify dependency: %w", err)
		}
		if !isRetryable(err) {
			// We didn't get a retryable serialization error, return.
			return nil, nil, fmt.Errorf("pg modify unknown: %w", err)
		}
		// Serialization error — back off randomly with increasing time caps.
		backoff := time.Duration(float64((1<<i)*minBackoff) * rand.Float64())
		if backoff > time.Second {
			backoff = time.Second
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("pg modify canceled during backoff: %w", ctx.Err())
		}
	}
	// Serialization errors that can't be retried are passed as empty
	// dependency errors. We don't know what the conflict was, but it was like
	// a dependency problem.
	return nil, nil, entroq.DependencyErrorf("retry limit: %v", err)
}

// modify calls the entroq_modify stored procedure, which atomically locks
// dependencies, checks versions, and performs all inserts/changes/deletes in
// one round trip. Returns a DependencyError (SQLSTATE EQ001) if any
// dependency constraint is violated.
func (b *backend) modify(ctx context.Context, mod *entroq.Modification) (inserted, changed []*entroq.Task, err error) {
	// Build parallel arrays for each operation set.
	depIDs, depVers := taskIDArrays(mod.Depends)
	delIDs, delVers := taskIDArrays(mod.Deletes)
	insIDs, insQueues, insAts, insValues, insAttempts, insErrs := insertArrays(mod.Inserts)
	chgIDs, chgVers, chgQueues, chgAts, chgValues, chgAttempts, chgErrs := changeArrays(mod.Changes)

	rows, err := b.db.QueryContext(ctx, `
		SELECT kind, id, version, queue, at, created, modified, claimant, value, claims, attempt, err
		FROM entroq_modify(
			$1,
			$2::uuid[], $3::integer[],
			$4::uuid[], $5::integer[],
			$6::uuid[], $7::text[], $8::timestamptz[], $9::bytea[], $10::integer[], $11::text[],
			$12::uuid[], $13::integer[], $14::text[], $15::timestamptz[], $16::bytea[], $17::integer[], $18::text[]
		)`,
		mod.Claimant,
		pq.Array(depIDs), pq.Array(depVers),
		pq.Array(delIDs), pq.Array(delVers),
		pq.Array(insIDs), pq.Array(insQueues), pq.Array(insAts), pq.ByteaArray(insValues), pq.Array(insAttempts), pq.Array(insErrs),
		pq.Array(chgIDs), pq.Array(chgVers), pq.Array(chgQueues), pq.Array(chgAts), pq.ByteaArray(chgValues), pq.Array(chgAttempts), pq.Array(chgErrs),
	)
	if err != nil {
		return nil, nil, parseModifyError(err, mod)
	}
	defer rows.Close()

	for rows.Next() {
		t := new(entroq.Task)
		var kind string
		if err := rows.Scan(&kind, &t.ID, &t.Version, &t.Queue, &t.At, &t.Created, &t.Modified, &t.Claimant, &t.Value, &t.Claims, &t.Attempt, &t.Err); err != nil {
			return nil, nil, fmt.Errorf("pg modify scan: %w", err)
		}
		switch kind {
		case "inserted":
			inserted = append(inserted, t)
		case "changed":
			changed = append(changed, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, parseModifyError(err, mod)
	}
	return inserted, changed, nil
}

// parseModifyError converts an EQ001 PostgreSQL error into a DependencyError,
// categorizing each affected task ID by which operation set it belongs to.
// Other errors are returned unchanged.
func parseModifyError(err error, mod *entroq.Modification) error {
	if err == nil {
		return nil
	}
	pgerr := new(pq.Error)
	if !errors.As(err, &pgerr) || string(pgerr.Code) != "EQ001" {
		return err
	}

	var detail struct {
		Missing    []struct {
			ID      uuid.UUID `json:"id"`
			Version int32     `json:"version"`
		} `json:"missing"`
		Mismatched []struct {
			ID      uuid.UUID `json:"id"`
			Version int32     `json:"version"`
		} `json:"mismatched"`
		Collisions []struct {
			ID      uuid.UUID `json:"id"`
			Version int32     `json:"version"`
		} `json:"collisions"`
	}
	if jsonErr := json.Unmarshal([]byte(pgerr.Detail), &detail); jsonErr != nil {
		return fmt.Errorf("pg modify EQ001 with unparseable detail %q: %w", pgerr.Detail, err)
	}

	// Build lookup sets to categorize IDs by operation.
	dependIDs := make(map[uuid.UUID]bool, len(mod.Depends))
	for _, t := range mod.Depends {
		dependIDs[t.ID] = true
	}
	deleteIDs := make(map[uuid.UUID]bool, len(mod.Deletes))
	for _, t := range mod.Deletes {
		deleteIDs[t.ID] = true
	}

	depErr := entroq.DependencyError{}

	categorize := func(id uuid.UUID, version int32) {
		tid := &entroq.TaskID{ID: id, Version: version}
		switch {
		case dependIDs[id]:
			depErr.Depends = append(depErr.Depends, tid)
		case deleteIDs[id]:
			depErr.Deletes = append(depErr.Deletes, tid)
		default:
			depErr.Changes = append(depErr.Changes, tid)
		}
	}

	for _, m := range detail.Missing {
		categorize(m.ID, m.Version)
	}
	for _, m := range detail.Mismatched {
		categorize(m.ID, m.Version)
	}
	for _, c := range detail.Collisions {
		depErr.Inserts = append(depErr.Inserts, &entroq.TaskID{ID: c.ID, Version: c.Version})
	}

	return depErr
}

// taskIDArrays splits a slice of TaskIDs into parallel UUID string and version slices.
func taskIDArrays(tids []*entroq.TaskID) (ids []string, versions []int32) {
	ids = make([]string, len(tids))
	versions = make([]int32, len(tids))
	for i, t := range tids {
		ids[i] = t.ID.String()
		versions[i] = t.Version
	}
	return
}

// insertArrays splits a slice of TaskData inserts into parallel arrays for the stored procedure.
func insertArrays(inserts []*entroq.TaskData) (ids []string, queues []string, ats []time.Time, values [][]byte, attempts []int32, errs []string) {
	ids = make([]string, len(inserts))
	queues = make([]string, len(inserts))
	ats = make([]time.Time, len(inserts))
	values = make([][]byte, len(inserts))
	attempts = make([]int32, len(inserts))
	errs = make([]string, len(inserts))
	for i, ins := range inserts {
		ids[i] = ins.ID.String() // uuid.Nil.String() signals auto-generate
		queues[i] = ins.Queue
		ats[i] = ins.At         // zero time signals use now()
		values[i] = ins.Value
		attempts[i] = ins.Attempt
		errs[i] = ins.Err
	}
	return
}

// changeArrays splits a slice of Task changes into parallel arrays for the stored procedure.
func changeArrays(changes []*entroq.Task) (ids []string, versions []int32, queues []string, ats []time.Time, values [][]byte, attempts []int32, errs []string) {
	ids = make([]string, len(changes))
	versions = make([]int32, len(changes))
	queues = make([]string, len(changes))
	ats = make([]time.Time, len(changes))
	values = make([][]byte, len(changes))
	attempts = make([]int32, len(changes))
	errs = make([]string, len(changes))
	for i, chg := range changes {
		ids[i] = chg.ID.String()
		versions[i] = chg.Version
		queues[i] = chg.Queue
		ats[i] = chg.At
		values[i] = chg.Value
		attempts[i] = chg.Attempt
		errs[i] = chg.Err
	}
	return
}

// Time returns the time used in all calculations in this process.
func (b *backend) Time(ctx context.Context) (time.Time, error) {
	row := b.db.QueryRowContext(ctx, "SELECT now()")
	var t time.Time
	if err := row.Scan(&t); err != nil {
		return time.Time{}, fmt.Errorf("postgres time: %w", err)
	}
	return t, nil
}
