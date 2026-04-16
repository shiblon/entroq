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
	"log"
	"math/rand"
	"net/url"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/subq"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
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
	db                string
	user              string
	password          string
	sslMode           string
	attempts          int
	readinessInterval time.Duration
	noListen          bool
	initSchema        bool
	nw                entroq.NotifyWaiter
	mp                metric.MeterProvider

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
		opts.sslMode = string(mode)
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

// WithInitSchema causes Open to initialize the database schema before opening
// the backend. Equivalent to calling InitSchema separately, but convenient for
// tests and single-binary deployments where a separate init step is unwanted.
// The schema DDL is idempotent, so this is safe to use on an already-initialized
// database.
func WithInitSchema() PGOpt {
	return func(opts *pgOptions) {
		opts.initSchema = true
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

// WithHeartbeat, when given a non-zero interval, pings a stored prcedure that
// triggers a notification for any queues containing recently-available tasks
// due to the passage of time. This can trigger some duplicate notifications,
// particularly for task modification, but these are generally harmless.
//
// In large clusters where multiple workers connect directly to postgres, it
// can be best to minimize the number of workers that emit heartbeats. Note that this
// does not work at all with connection pool proxies, so should be disabled by
// setting interval = 0.
func WithHeartbeat(interval time.Duration) PGOpt {
	return func(opts *pgOptions) {
		opts.readinessInterval = interval
	}
}

// WithNoListen disables the dedicated PostgreSQL LISTEN connection.
// This means that the only notifications received will be in-process. This
// works very well for a singleton service that is basically the only thing
// talking to the Postgres backend. No need for a network round trip.
//
// If, however, there are multiple things talking to a postgres backend, the
// NOTIFY/LISTEN approach in postgres can notify all of them. WithNoListen turns
// that off, so multiple clients of postgres can't wake up if another client
// does something to a queue they are watching.
//
// The gist is that in single-server scenarios, where PostgreSQL is an
// implementation detail behind an RPC service, you can use this to turn off
// the listener. In situations where this is one of several clients of
// PostgreSQL, leave it on.
func WithNoListen() PGOpt {
	return func(opts *pgOptions) {
		opts.noListen = true
	}
}

// WithMeterProvider sets the OTel MeterProvider for claim and modify duration
// histograms. Defaults to a noop provider.
func WithMeterProvider(mp metric.MeterProvider) PGOpt {
	return func(opts *pgOptions) {
		opts.mp = mp
	}
}

// Open opens a postgres backend with the given host/port and options. This is
// useful if you want to get at eqpg-specific backend options like
// in-transaction database updates.
// buildConnStr constructs a libpq connection string from a host:port and options.
func buildConnStr(hostPort string, options *pgOptions) (string, error) {
	u, err := url.Parse("postgres://" + hostPort)
	if err != nil {
		return "", fmt.Errorf("failed to parse hostport %q: %w", hostPort, err)
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
		"sslmode=" + options.sslMode,
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
	params = append(params, "search_path=entroq,public")
	return strings.Join(params, " "), nil
}

// defaultOptions returns a pgOptions with the standard defaults applied.
func defaultOptions(opts []PGOpt) *pgOptions {
	options := &pgOptions{
		db:                "postgres",
		user:              "postgres",
		password:          "password",
		attempts:          1,
		sslMode:           string(SSLDisable),
		readinessInterval: 0, // 0 == no heartbeat
		noListen:          false,
	}
	for _, o := range opts {
		o(options)
	}
	return options
}

// OpenDB opens a *sql.DB using the given connection parameters without
// performing any schema version check. Use this when the schema may not yet
// exist or may be in a legacy state -- e.g. for schema init, upgrade, or
// version commands. Open is the right choice for normal service use.
func OpenDB(hostPort string, opts ...PGOpt) (*sql.DB, error) {
	options := defaultOptions(opts)
	connStr, err := buildConnStr(hostPort, options)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	return db, nil
}

// Open opens a fully operational *EQPG backend, verifying that the database
// schema is present and at the expected version. Fails loudly if the schema is
// uninitialized or at the wrong version; run "eqpg schema init" or
// "eqpg schema upgrade" first.
func Open(ctx context.Context, hostPort string, opts ...PGOpt) (*EQPG, error) {
	options := defaultOptions(opts)
	connStr, err := buildConnStr(hostPort, options)
	if err != nil {
		return nil, err
	}

	db, err := OpenDB(hostPort, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to open postgres DB: %w", err)
	}

	if options.nw == nil {
		if options.noListen {
			// Don't rely on database notify/listen, use internal one instead.
			options.nw = subq.New()
		} else {
			// Otherwise use the PG-aware mechanism.
			options.nw = NewPGNotifyWaiter(connStr)
		}
	}

	for i := 0; i < options.attempts; i++ {
		if err = db.PingContext(ctx); err == nil {
			if options.initSchema {
				if err := InitSchema(ctx, db); err != nil {
					return nil, fmt.Errorf("pg open init schema: %w", err)
				}
			}
			return New(ctx, db, options.nw, options)
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

// Opener creates an opener function to be used to get a backend.
// If you need some of the database-specific options in this module, use Open
// instead and pass the resulting backend into entroq.New.
func Opener(hostPort string, opts ...PGOpt) entroq.BackendOpener {
	return func(ctx context.Context) (entroq.Backend, error) {
		return Open(ctx, hostPort, opts...)
	}
}

type EQPG struct {
	DB *sql.DB
	nw entroq.NotifyWaiter

	stopTicker     func()
	claimDuration  metric.Float64Histogram
	modifyDuration metric.Float64Histogram
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
func New(ctx context.Context, db *sql.DB, nw entroq.NotifyWaiter, opts *pgOptions) (*EQPG, error) {
	b := &EQPG{
		DB: db,
		nw: nw,
	}

	err := b.initDB(ctx)
	if err == io.EOF {
		return nil, io.EOF
	}
	if err != nil {
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}

	if opts.readinessInterval > 0 {
		tickerCtx, stop := context.WithCancel(ctx)
		b.stopTicker = stop
		go b.runReadinessTicker(tickerCtx, opts.readinessInterval)
	}

	mp := opts.mp
	if mp == nil {
		mp = noop.NewMeterProvider()
	}
	if err := b.initMetrics(mp); err != nil {
		return nil, fmt.Errorf("eqpg init metrics: %w", err)
	}

	return b, nil
}

func (b *EQPG) initMetrics(mp metric.MeterProvider) error {
	meter := mp.Meter("entroq.pg")
	var err error
	b.claimDuration, err = meter.Float64Histogram("entroq.claim.duration",
		metric.WithDescription("Duration of TryClaim calls against the database."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return fmt.Errorf("claim duration histogram: %w", err)
	}
	b.modifyDuration, err = meter.Float64Histogram("entroq.modify.duration",
		metric.WithDescription("Duration of Modify calls against the database."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return fmt.Errorf("modify duration histogram: %w", err)
	}
	return nil
}

// Close closes the underlying database connection.
func (b *EQPG) Close() error {
	if b.stopTicker != nil {
		b.stopTicker()
	}
	if err := b.DB.Close(); err != nil {
		return fmt.Errorf("pg backend close: %w", err)
	}
	return nil
}

// runReadinessTicker examines the task table to see what queues had tasks
// become recently available to notify on them for the passage of time.
func (b *EQPG) runReadinessTicker(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// notify_ready_queues atomically updates its own watermark state.
			// We use a safety interval of half the ticker interval to prevent
			// accidental double-flushing if tickers drift.
			rows, err := b.DB.QueryContext(ctx, "SELECT entroq.notify_ready_queues($1)", fmt.Sprintf("%d microseconds", interval/(2*time.Microsecond)))
			if err != nil {
				log.Printf("pg readiness ticker: %v", err)
				continue
			}

			// Bridge: Forward global ready-event to the local waiter.
			for rows.Next() {
				var q string
				if err := rows.Scan(&q); err != nil {
					log.Printf("pg readiness scan: %v", err)
					continue
				}
				if b.nw != nil {
					b.nw.Notify(q)
				}
			}
			rows.Close()
		}
	}
}

// Queues returns the queues and their sizes.
func (b *EQPG) Queues(ctx context.Context, qq *entroq.QueuesQuery) (map[string]int, error) {
	return entroq.QueuesFromStats(b.QueueStats(ctx, qq))
}

// QueueStats returns a mapping from queue names to their statistics.
func (b *EQPG) QueueStats(ctx context.Context, qq *entroq.QueuesQuery) (map[string]*entroq.QueueStat, error) {
	// Hybrid Strategy:
	// We use Index-Only Scans for all metrics. These are read-only and non-blocking.
	// 1. Total: O(N) index-only scan (fast but scale-dependent).
	// 2. Claimed: Fast index-only range scan (at > now AND claimant != '').
	// 3. Available: Fast index-only range scan (at <= now AND claimant = '').
	// 4. MaxClaims: O(1) index-seek per queue via byQueueClaims index.
	q := `SELECT
			queue,
			COUNT(*) AS count,
			COUNT(*) FILTER(WHERE at > NOW() AND claims > 0) AS claimed,
			COUNT(*) FILTER(WHERE at > NOW() AND claims = 0) AS future,
			COUNT(*) FILTER(WHERE at <= NOW()) as available,
			COALESCE(MAX(claims), 0) AS max_claims
		FROM entroq.tasks`
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

	q += " GROUP BY queue"

	if qq.Limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", len(values)+1)
		values = append(values, qq.Limit)
	}

	rows, err := b.DB.QueryContext(ctx, q, values...)
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
			future    int
			available int
			maxClaims int
		)
		if err := rows.Scan(&q, &count, &claimed, &future, &available, &maxClaims); err != nil {
			return nil, fmt.Errorf("queue names scan: %w", err)
		}
		queues[q] = &entroq.QueueStat{
			Name:      q,
			Size:      count,
			Claimed:   claimed,
			Future:    future,
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
func (b *EQPG) Tasks(ctx context.Context, tq *entroq.TasksQuery) ([]*entroq.Task, error) {
	q := "SELECT id, version, queue, at, created, modified, claimant, value, claims, attempt, err FROM tasks WHERE true"
	var values []interface{}

	if tq.Queue != "" {
		q += fmt.Sprintf(" AND queue = $%d", len(values)+1)
		values = append(values, tq.Queue)
	}

	if tq.Claimant != "" {
		q += fmt.Sprintf(" AND (claimant = $%d OR claimant = $%d OR at < NOW())", len(values)+1, len(values)+2)
		values = append(values, "", tq.Claimant)
	}

	// Add IDs if a set of limiting IDs has been requested.
	strIDs := make([]string, 0, len(tq.IDs))
	for _, id := range tq.IDs {
		strIDs = append(strIDs, id)
	}
	if len(strIDs) != 0 {
		q += fmt.Sprintf(" AND id = any($%d)", len(values)+1)
		values = append(values, pq.StringArray(strIDs))
	}

	if tq.Limit > 0 {
		// Safe to directly append, since it's an int.
		q += fmt.Sprintf(" LIMIT %d", tq.Limit)
	}

	rows, err := b.DB.QueryContext(ctx, q, values...)
	if err != nil {
		return nil, fmt.Errorf("queue tasks %q: %w", tq.Queue, err)
	}
	defer rows.Close()
	var tasks []*entroq.Task
	for rows.Next() {
		t := &entroq.Task{}
		var val []byte
		if err := rows.Scan(&t.ID, &t.Version, &t.Queue, &t.At, &t.Created, &t.Modified, &t.Claimant, &val, &t.Claims, &t.Attempt, &t.Err); err != nil {
			return nil, fmt.Errorf("task scan: %w", err)
		}
		t.Value = val
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
func (b *EQPG) Claim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	if b.nw != nil {
		return entroq.WaitTryClaim(ctx, cq, b.TryClaim, b.nw)
	}
	return entroq.PollTryClaim(ctx, cq, b.TryClaim)
}

// TryClaim attempts to claim an "arrived" task from any of the specified
// queues, attempting to do so fairly across queues. Returns a nil task (no
// error) if all queues are empty.
func (b *EQPG) TryClaim(ctx context.Context, cq *entroq.ClaimQuery) (*entroq.Task, error) {
	if cq.Duration == 0 {
		return nil, fmt.Errorf("no duration set for claim %q", cq.Queues)
	}
	start := time.Now()
	defer func() {
		b.claimDuration.Record(ctx, time.Since(start).Seconds())
	}()
	task := new(entroq.Task)
	var val []byte
	err := b.DB.QueryRowContext(ctx,
		`SELECT id, version, queue, at, created, modified, claimant, value, claims, attempt, err
		 FROM try_claim($1, $2, $3)`,
		pq.Array(cq.Queues), cq.Claimant, fmt.Sprintf("%d microseconds", cq.Duration/time.Microsecond),
	).Scan(
		&task.ID, &task.Version, &task.Queue, &task.At,
		&task.Created, &task.Modified, &task.Claimant,
		&val, &task.Claims, &task.Attempt, &task.Err,
	)
	task.Value = val
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

// modifyConfig holds options for how Modify should execute.
type modifyConfig struct {
	runInTx func(context.Context, *sql.Tx) error
}

// modOpt is a private type for options that only this backend understands.
// It satisfies the entroq.ModifyOption interface so that it can be passed
// there.
type modOpt func(c *modifyConfig)

// IsModifyBackend returns nil if b is an *EQPG, or a descriptive error otherwise.
// Its presence also causes modOpt to satisfy the entroq.ModifyOption interface.
func (modOpt) IsModifyBackend(b entroq.Backend) error {
	if _, ok := b.(*EQPG); !ok {
		return fmt.Errorf("requires a PostgreSQL (*eqpg.EQPG) backend, got %T", b)
	}
	return nil
}

// RunningInTx returns an entroq.ModifyOption that signals to this backend
// to run f inside the Modify transaction.
//
// Important: the callback is responsible for managing any rows returned,
// including closing them before the callback completes.
func RunningInTx(f func(context.Context, *sql.Tx) error) entroq.ModifyOption {
	return modOpt(func(c *modifyConfig) {
		c.runInTx = f
	})
}

// Modify attempts to apply an atomic modification to the task store. Either
// all succeeds or all fails.
func (b *EQPG) Modify(ctx context.Context, mod *entroq.Modification) (*entroq.ModifyResponse, error) {
	start := time.Now()
	defer func() {
		b.modifyDuration.Record(ctx, time.Since(start).Seconds())
	}()
	options := &modifyConfig{}
	for _, o := range mod.Options() {
		if pgOpt, ok := o.(modOpt); ok {
			pgOpt(options)
		}
	}
	return b.modifyHandlingRetriable(ctx, func() (*entroq.ModifyResponse, error) {
		return b.modify(ctx, mod, options)
	})
}

// modifyHandlingRetriable runs a retry loop for handling database retriable errors.
func (b *EQPG) modifyHandlingRetriable(ctx context.Context, doModify func() (*entroq.ModifyResponse, error)) (*entroq.ModifyResponse, error) {
	const minBackoff = 10 * time.Millisecond
	var err error
	for i := 0; i < 7; i++ {
		var resp *entroq.ModifyResponse
		resp, err = doModify()
		// No error - we're done!
		if err == nil {
			// Notify any waiters of tasks that were just changed/inserted that are
			// ready to go.
			if b.nw != nil {
				entroq.NotifyModified(b.nw, resp.InsertedTasks, resp.ChangedTasks)
			}
			return resp, nil
		}
		if _, ok := entroq.AsDependency(err); ok {
			// We know what's wrong, no reason to retry.
			return nil, fmt.Errorf("pg modify dependency: %w", err)
		}
		if !isRetryable(err) {
			// We didn't get a retryable serialization error, return.
			return nil, fmt.Errorf("pg modify unknown: %w", err)
		}
		// Serialization error -- back off randomly with increasing time caps.
		backoff := time.Duration(float64((1<<i)*minBackoff) * rand.Float64())
		if backoff > time.Second {
			backoff = time.Second
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, fmt.Errorf("pg modify canceled during backoff: %w", ctx.Err())
		}
	}
	// Serialization errors that can't be retried are passed as empty
	// dependency errors. We don't know what the conflict was, but it was like
	// a dependency problem.
	return nil, entroq.DependencyErrorf("retry limit: %v", err)
}

// modify calls the modify_arrays stored procedure, which atomically locks
// dependencies, checks versions, and performs all inserts/changes/deletes in
// one round trip. Returns a DependencyError (SQLSTATE EQ001) if any
// dependency constraint is violated.
func (b *EQPG) modify(ctx context.Context, mod *entroq.Modification, options *modifyConfig) (*entroq.ModifyResponse, error) {
	// Build parallel arrays for task operation set.
	depIDs, depVers := taskIDArrays(mod.Depends)
	delIDs, delVers := taskIDArrays(mod.Deletes)
	insIDs, insQueues, insAts, insValues, insAttempts, insErrs := insertArrays(mod.Inserts)
	chgIDs, chgVers, chgQueues, chgAts, chgValues, chgAttempts, chgErrs := changeArrays(mod.Changes)

	// Build parallel arrays for resource operation set.
	rDepNS, rDepIDs, rDepVers := resourceIDArrays(mod.DocDepends)
	rDelNS, rDelIDs, rDelVers := resourceIDArrays(mod.DocDeletes)
	rInsNS, rInsIDs, rInsPKeys, rInsSKeys, rInsValues := resourceInsertArrays(mod.DocInserts)
	rChgNS, rChgIDs, rChgVers, rChgPKeys, rChgSKeys, rChgValues, rChgAts := resourceChangeArrays(mod.DocChanges)

	if options == nil {
		options = &modifyConfig{}
	}

	tx, err := b.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("pg modify begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				err = fmt.Errorf("pg modify rollback failed: %v (original error: %w)", rbErr, err)
			}
		} else {
			if cmErr := tx.Commit(); cmErr != nil {
				err = fmt.Errorf("pg modify commit failed: %w", cmErr)
			}
		}
	}()

	// Run caller's DB work first, inside the same transaction, if specified.
	if options.runInTx != nil {
		if err := options.runInTx(ctx, tx); err != nil {
			return nil, fmt.Errorf("pg modify caller tx work: %w", err)
		}
	}

	resp := new(entroq.ModifyResponse)

	// Resource modifications.
	if len(rDepIDs)+len(rDelIDs)+len(rInsIDs)+len(rChgIDs) > 0 {
		rRows, err := tx.QueryContext(ctx, `
			SELECT kind, namespace, id, version, claimant, at, key_primary, key_secondary, value, created, modified
			FROM _modify_docs(
				$1,
				$2::text[], $3::text[], $4::integer[],
				$5::text[], $6::text[], $7::integer[],
				$8::text[], $9::text[], $10::text[], $11::text[], $12::text[],
				$13::text[], $14::text[], $15::integer[], $16::text[], $17::text[], $18::text[], $19::timestamptz[]
			)`,
			mod.Claimant,
			pq.Array(rDepNS), pq.Array(rDepIDs), pq.Array(rDepVers),
			pq.Array(rDelNS), pq.Array(rDelIDs), pq.Array(rDelVers),
			pq.Array(rInsNS), pq.Array(rInsIDs), pq.Array(rInsPKeys), pq.Array(rInsSKeys), pq.Array(rInsValues),
			pq.Array(rChgNS), pq.Array(rChgIDs), pq.Array(rChgVers), pq.Array(rChgPKeys), pq.Array(rChgSKeys), pq.Array(rChgValues), pq.Array(rChgAts),
		)
		if err != nil {
			return nil, parseModifyDocsError(err, mod)
		}
		defer rRows.Close()

		for rRows.Next() {
			r := new(entroq.Doc)
			var kind string
			var val []byte
			var claimant sql.NullString
			var at, created, modified sql.NullTime
			if err := rRows.Scan(&kind, &r.Namespace, &r.ID, &r.Version, &claimant, &at, &r.Key, &r.SecondaryKey, &val, &created, &modified); err != nil {
				return nil, fmt.Errorf("pg modify resource scan: %w", err)
			}
			r.Claimant = claimant.String
			r.Content = val
			r.At = at.Time
			r.Created = created.Time
			r.Modified = modified.Time
			switch kind {
			case "inserted":
				resp.InsertedDocs = append(resp.InsertedDocs, r)
			case "changed":
				resp.ChangedDocs = append(resp.ChangedDocs, r)
			}
		}
	}

	// Task modifications.
	rows, err := tx.QueryContext(ctx, `
		SELECT kind, id, version, queue, at, created, modified, claimant, value, claims, attempt, err
		FROM _modify_arrays(
			$1,
			$2::text[], $3::integer[],
			$4::text[], $5::integer[],
			$6::text[], $7::text[], $8::timestamptz[], $9::text[], $10::integer[], $11::text[],
			$12::text[], $13::integer[], $14::text[], $15::timestamptz[], $16::text[], $17::integer[], $18::text[]
		)`,
		mod.Claimant,
		pq.Array(depIDs), pq.Array(depVers),
		pq.Array(delIDs), pq.Array(delVers),
		pq.Array(insIDs), pq.Array(insQueues), pq.Array(insAts), pq.Array(insValues), pq.Array(insAttempts), pq.Array(insErrs),
		pq.Array(chgIDs), pq.Array(chgVers), pq.Array(chgQueues), pq.Array(chgAts), pq.Array(chgValues), pq.Array(chgAttempts), pq.Array(chgErrs),
	)
	if err != nil {
		return nil, parseModifyError(err, mod)
	}
	defer rows.Close()

	for rows.Next() {
		t := new(entroq.Task)
		var kind string
		var val []byte
		var claimant sql.NullString
		var at, created, modified sql.NullTime
		if err := rows.Scan(&kind, &t.ID, &t.Version, &t.Queue, &at, &created, &modified, &claimant, &val, &t.Claims, &t.Attempt, &t.Err); err != nil {
			return nil, fmt.Errorf("pg modify task scan: %w", err)
		}
		t.Value = val
		t.Claimant = claimant.String
		t.At = at.Time
		t.Created = created.Time
		t.Modified = modified.Time
		switch kind {
		case "inserted":
			resp.InsertedTasks = append(resp.InsertedTasks, t)
		case "changed":
			resp.ChangedTasks = append(resp.ChangedTasks, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, parseModifyError(err, mod)
	}
	return resp, nil
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
		Missing []struct {
			ID      string `json:"id"`
			Version int32  `json:"version"`
		} `json:"missing"`
		Mismatched []struct {
			ID      string `json:"id"`
			Version int32  `json:"version"`
		} `json:"mismatched"`
		Collisions []struct {
			ID      string `json:"id"`
			Version int32  `json:"version"`
		} `json:"collisions"`
	}
	if jsonErr := json.Unmarshal([]byte(pgerr.Detail), &detail); jsonErr != nil {
		return fmt.Errorf("pg modify EQ001 with unparseable detail %q: %w", pgerr.Detail, err)
	}

	// Build lookup sets to categorize IDs by operation.
	dependIDs := make(map[string]bool, len(mod.Depends))
	for _, t := range mod.Depends {
		dependIDs[t.ID] = true
	}
	deleteIDs := make(map[string]bool, len(mod.Deletes))
	for _, t := range mod.Deletes {
		deleteIDs[t.ID] = true
	}

	depErr := new(entroq.DependencyError)

	categorize := func(id string, version int32) {
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

// parseModifyDocsError converts an EQ001 PostgreSQL error from _modify_docs
// into a DependencyError with doc fields populated. The doc error JSON uses
// {ns, id, version} for missing/mismatched and {ns, id} for collisions,
// unlike the task error which uses {id, version}. Other errors are returned unchanged.
func parseModifyDocsError(err error, mod *entroq.Modification) error {
	if err == nil {
		return nil
	}
	pgerr := new(pq.Error)
	if !errors.As(err, &pgerr) || string(pgerr.Code) != "EQ001" {
		return err
	}

	var detail struct {
		Missing []struct {
			NS      string `json:"ns"`
			ID      string `json:"id"`
			Version int32  `json:"version"`
		} `json:"missing"`
		Mismatched []struct {
			NS      string `json:"ns"`
			ID      string `json:"id"`
			Version int32  `json:"version"`
		} `json:"mismatched"`
		Collisions []struct {
			NS string `json:"ns"`
			ID string `json:"id"`
		} `json:"collisions"`
	}
	if jsonErr := json.Unmarshal([]byte(pgerr.Detail), &detail); jsonErr != nil {
		return fmt.Errorf("pg modify docs EQ001 with unparseable detail %q: %w", pgerr.Detail, err)
	}

	// Build lookup sets to categorize (namespace, id) pairs by operation.
	dependKeys := make(map[string]bool, len(mod.DocDepends))
	for _, d := range mod.DocDepends {
		dependKeys[entroq.DocKey(d.Namespace, d.ID)] = true
	}
	deleteKeys := make(map[string]bool, len(mod.DocDeletes))
	for _, d := range mod.DocDeletes {
		deleteKeys[entroq.DocKey(d.Namespace, d.ID)] = true
	}

	depErr := new(entroq.DependencyError)

	categorize := func(ns, id string, version int32) {
		did := &entroq.DocID{Namespace: ns, ID: id, Version: version}
		k := entroq.DocKey(ns, id)
		switch {
		case dependKeys[k]:
			depErr.DocDepends = append(depErr.DocDepends, did)
		case deleteKeys[k]:
			depErr.DocDeletes = append(depErr.DocDeletes, did)
		default:
			depErr.DocChanges = append(depErr.DocChanges, did)
		}
	}

	for _, m := range detail.Missing {
		categorize(m.NS, m.ID, m.Version)
	}
	for _, m := range detail.Mismatched {
		categorize(m.NS, m.ID, m.Version)
	}
	for _, c := range detail.Collisions {
		depErr.DocInserts = append(depErr.DocInserts, &entroq.DocID{Namespace: c.NS, ID: c.ID})
	}

	return depErr
}

// taskIDArrays splits a slice of TaskIDs into parallel ID string and version slices.
func taskIDArrays(tids []*entroq.TaskID) (ids []string, versions []int32) {
	ids = make([]string, len(tids))
	versions = make([]int32, len(tids))
	for i, t := range tids {
		ids[i] = t.ID
		versions[i] = t.Version
	}
	return
}

// jsonTextVal converts a json.RawMessage to a *string for use in a text[] SQL
// parameter. nil produces nil (SQL NULL); non-nil produces the JSON text.
func jsonTextVal(v json.RawMessage) *string {
	if v == nil {
		return nil
	}
	s := string(v)
	return &s
}

// insertArrays splits a slice of TaskData inserts into parallel arrays for the stored procedure.
func insertArrays(inserts []*entroq.TaskData) (ids []string, queues []string, ats []time.Time, values []*string, attempts []int32, errs []string) {
	ids = make([]string, len(inserts))
	queues = make([]string, len(inserts))
	ats = make([]time.Time, len(inserts))
	values = make([]*string, len(inserts))
	attempts = make([]int32, len(inserts))
	errs = make([]string, len(inserts))
	for i, ins := range inserts {
		ids[i] = ins.ID // empty signals auto-generate, the common case
		queues[i] = ins.Queue
		ats[i] = ins.At // zero time signals use now()
		values[i] = jsonTextVal(ins.Value)
		attempts[i] = ins.Attempt
		errs[i] = ins.Err
	}
	return
}

// changeArrays splits a slice of Task changes into parallel arrays for the stored procedure.
func changeArrays(changes []*entroq.Task) (ids []string, versions []int32, queues []string, ats []time.Time, values []*string, attempts []int32, errs []string) {
	ids = make([]string, len(changes))
	versions = make([]int32, len(changes))
	queues = make([]string, len(changes))
	ats = make([]time.Time, len(changes))
	values = make([]*string, len(changes))
	attempts = make([]int32, len(changes))
	errs = make([]string, len(changes))
	for i, chg := range changes {
		ids[i] = chg.ID
		versions[i] = chg.Version
		queues[i] = chg.Queue
		ats[i] = chg.At
		values[i] = jsonTextVal(chg.Value)
		attempts[i] = chg.Attempt
		errs[i] = chg.Err
	}
	return
}

// resourceIDArrays splits a slice of ResourceIDs into parallel arrays.
func resourceIDArrays(rids []*entroq.DocID) (ns, ids []string, versions []int32) {
	ns = make([]string, len(rids))
	ids = make([]string, len(rids))
	versions = make([]int32, len(rids))
	for i, r := range rids {
		ns[i] = r.Namespace
		ids[i] = r.ID
		versions[i] = r.Version
	}
	return
}

// resourceInsertArrays splits a slice of ResourceData into parallel arrays.
func resourceInsertArrays(inserts []*entroq.DocData) (ns, ids, pkeys, skeys []string, values []*string) {
	ns = make([]string, len(inserts))
	ids = make([]string, len(inserts))
	pkeys = make([]string, len(inserts))
	skeys = make([]string, len(inserts))
	values = make([]*string, len(inserts))
	for i, ins := range inserts {
		ns[i] = ins.Namespace
		ids[i] = ins.ID
		pkeys[i] = ins.Key
		skeys[i] = ins.SecondaryKey
		values[i] = jsonTextVal(ins.Content)
	}
	return
}

// resourceChangeArrays splits a slice of Resource changes into parallel arrays.
func resourceChangeArrays(changes []*entroq.Doc) (ns, ids []string, versions []int32, pkeys, skeys []string, values []*string, ats []time.Time) {
	ns = make([]string, len(changes))
	ids = make([]string, len(changes))
	versions = make([]int32, len(changes))
	pkeys = make([]string, len(changes))
	skeys = make([]string, len(changes))
	values = make([]*string, len(changes))
	ats = make([]time.Time, len(changes))
	for i, chg := range changes {
		ns[i] = chg.Namespace
		ids[i] = chg.ID
		versions[i] = chg.Version
		pkeys[i] = chg.Key
		skeys[i] = chg.SecondaryKey
		values[i] = jsonTextVal(chg.Content)
		ats[i] = chg.At
	}
	return
}

// Time returns the time used in all calculations in this process.
func (b *EQPG) Time(ctx context.Context) (time.Time, error) {
	row := b.DB.QueryRowContext(ctx, "SELECT now()")
	var t time.Time
	if err := row.Scan(&t); err != nil {
		return time.Time{}, fmt.Errorf("postgres time: %w", err)
	}
	return t, nil
}

// scanDocRows scans all rows from a doc query into a slice of Doc.
func scanDocRows(rows *sql.Rows) ([]*entroq.Doc, error) {
	var results []*entroq.Doc
	for rows.Next() {
		r := new(entroq.Doc)
		var val []byte
		var claimant sql.NullString
		var at, created, modified sql.NullTime
		if err := rows.Scan(&r.Namespace, &r.ID, &r.Version, &claimant, &at, &r.Key, &r.SecondaryKey, &val, &created, &modified); err != nil {
			return nil, fmt.Errorf("pg docs scan: %w", err)
		}
		r.Content = val
		r.Claimant = claimant.String
		r.At = at.Time
		r.Created = created.Time
		r.Modified = modified.Time
		results = append(results, r)
	}
	return results, rows.Err()
}

// Docs returns docs in a namespace. If IDs are specified, only those docs are
// returned (key range and limit are ignored). Otherwise, docs are filtered by
// optional key range and subject to limit.
func (b *EQPG) Docs(ctx context.Context, rq *entroq.DocQuery) ([]*entroq.Doc, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if len(rq.IDs) > 0 {
		rows, err = b.DB.QueryContext(ctx,
			`SELECT namespace, id, version, claimant, at, key_primary, key_secondary, value, created, modified
			 FROM entroq.docs
			 WHERE (namespace = $1 OR $1 = '') AND id = ANY($2)
			 ORDER BY namespace, key_primary, key_secondary`,
			rq.Namespace, pq.StringArray(rq.IDs),
		)
	} else {
		rows, err = b.DB.QueryContext(ctx,
			`SELECT namespace, id, version, claimant, at, key_primary, key_secondary, value, created, modified
			 FROM entroq.docs($1, $2, $3, $4, $5)`,
			rq.Namespace, rq.KeyStart, rq.KeyEnd, rq.Limit, rq.OmitValues,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("pg docs query: %w", err)
	}
	defer rows.Close()
	return scanDocRows(rows)
}

// ClaimDocs claims all docs with the given primary key in the namespace.
// Returns a DependencyError if any doc with that key is already claimed by
// another claimant. Returns an empty slice (not an error) if none exist.
func (b *EQPG) ClaimDocs(ctx context.Context, cq *entroq.DocClaim) ([]*entroq.Doc, error) {
	if err := cq.Validate(); err != nil {
		return nil, fmt.Errorf("claim docs: %w", err)
	}

	dur := fmt.Sprintf("%d microseconds", cq.Duration.Microseconds())
	rows, err := b.DB.QueryContext(ctx,
		`SELECT namespace, id, version, claimant, at, key_primary, key_secondary, value, created, modified
		 FROM entroq._claim_docs($1, $2, $3::interval, $4)`,
		cq.Namespace, cq.Claimant, dur, cq.Key,
	)
	if err != nil {
		return nil, parseClaimDocsError(err)
	}
	defer rows.Close()
	results, err := scanDocRows(rows)
	if err != nil {
		return nil, parseClaimDocsError(err)
	}
	return results, nil
}

// parseClaimDocsError converts an EQ001 from _claim_docs into a DependencyError
// with DocDeletes (missing docs) and DocClaims (already-claimed docs) populated.
// Other errors are returned unchanged.
func parseClaimDocsError(err error) error {
	if err == nil {
		return nil
	}
	pgerr := new(pq.Error)
	if !errors.As(err, &pgerr) || string(pgerr.Code) != "EQ001" {
		return fmt.Errorf("pg claim docs: %w", err)
	}

	var detail struct {
		MissingDocs []struct {
			Namespace string `json:"namespace"`
			ID        string `json:"id"`
			Version   int32  `json:"version"`
		} `json:"missing_docs"`
		ClaimedDocs []struct {
			Namespace string `json:"namespace"`
			ID        string `json:"id"`
			Version   int32  `json:"version"`
		} `json:"claimed_docs"`
	}
	if jsonErr := json.Unmarshal([]byte(pgerr.Detail), &detail); jsonErr != nil {
		return fmt.Errorf("pg claim docs EQ001 with unparseable detail %q: %w", pgerr.Detail, err)
	}

	depErr := new(entroq.DependencyError)
	for _, d := range detail.MissingDocs {
		depErr.DocDeletes = append(depErr.DocDeletes, &entroq.DocID{
			Namespace: d.Namespace,
			ID:        d.ID,
			Version:   d.Version,
		})
	}
	for _, d := range detail.ClaimedDocs {
		depErr.DocClaims = append(depErr.DocClaims, &entroq.DocID{
			Namespace: d.Namespace,
			ID:        d.ID,
			Version:   d.Version,
		})
	}
	return depErr
}
