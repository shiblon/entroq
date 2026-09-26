// Package eqpg provides an entroq.Backend using PostgreSQL. Use Opener with
// entroq.New to create a task client that talks to a PostgreSQL backend.
//
// # Garbage collection
//
// This backend garbage-collects on its own. Queues and doc namespaces that opt
// in by name (a /gc= component) have their arrived tasks or complete unclaimed
// doc groups reaped by an always-on background loop started when the backend is
// opened. It is a first-class backend behavior, not a separate process, so a
// client talking directly to PostgreSQL with this package (the many-clients,
// one-database model, with no "eqpg serve" in front) collects gc=-marked queues
// -- async response queues, eqlink dedup tombstones, and the like -- exactly as
// a server does, with no side process or configuration required.
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

	"github.com/lib/pq"
	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/gcmetrics"
	"github.com/shiblon/entroq/pkg/backend/internal/validate"
	"github.com/shiblon/entroq/pkg/internal/latency"
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
	initSchema        bool
	nw                entroq.NotifyWaiter
	mp                metric.MeterProvider

	gcInterval  time.Duration
	gcBatchSize int

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

// WithMeterProvider sets the OTel MeterProvider for claim and modify duration
// histograms. Defaults to a noop provider.
func WithMeterProvider(mp metric.MeterProvider) PGOpt {
	return func(opts *pgOptions) {
		opts.mp = mp
	}
}

// buildConnStr constructs a libpq connection string from a host:port and
// options, or prepares a complete PostgreSQL URL. A URL is authoritative for
// connection parameters; PGOpts still configure backend behavior such as
// connection attempts and notifications.
func buildConnStr(target string, options *pgOptions) (string, error) {
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil {
			var urlErr *url.Error
			if errors.As(err, &urlErr) {
				err = urlErr.Err
			}
			return "", fmt.Errorf("parse PostgreSQL URL: %w", err)
		}
		if u.Scheme != "postgres" && u.Scheme != "postgresql" {
			return "", fmt.Errorf("unsupported PostgreSQL URL scheme %q", u.Scheme)
		}

		query := u.Query()
		query.Set("search_path", "entroq,public")
		u.RawQuery = query.Encode()
		return u.String(), nil
	}

	hostPort := target
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
		params = append(params, "sslkey="+escp(options.sslClientKeyFile))
	}
	if options.sslClientCertFile != "" {
		params = append(params, "sslcert="+escp(options.sslClientCertFile))
	}
	if options.sslServerCAFile != "" {
		params = append(params, "sslrootcert="+escp(options.sslServerCAFile))
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
		readinessInterval: DefaultReadinessInterval,
		gcInterval:        defaultGCInterval,
		gcBatchSize:       defaultGCBatchSize,
	}
	for _, o := range opts {
		o(options)
	}
	return options
}

// OpenDB opens a *sql.DB using a host:port plus connection options or a complete
// PostgreSQL URL, without performing any schema version check. Use this when the
// schema may not yet exist or may be in a legacy state -- e.g. for schema init,
// upgrade, or version commands. When target is a URL, it supplies all connection
// parameters and connection-related options are ignored. Open is the right
// choice for normal service use.
func OpenDB(target string, opts ...PGOpt) (*sql.DB, error) {
	options := defaultOptions(opts)
	connStr, err := buildConnStr(target, options)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	return db, nil
}

// Open opens a fully operational *EQPG backend from a host:port or complete
// PostgreSQL URL, verifying that the database schema is present and at the
// expected version. Fails loudly if the schema is uninitialized or at the wrong
// version; run "eqpg schema init" or "eqpg schema upgrade" first.
func Open(ctx context.Context, target string, opts ...PGOpt) (b *EQPG, err error) {
	options := defaultOptions(opts)

	db, err := OpenDB(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to open postgres DB: %w", err)
	}
	// The backend owns db once New succeeds; until then, Open does.
	defer func() {
		if err != nil {
			db.Close()
		}
	}()

	if options.nw == nil {
		options.nw = subq.New()
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
func Opener(target string, opts ...PGOpt) entroq.BackendOpener {
	return func(ctx context.Context) (entroq.Backend, error) {
		return Open(ctx, target, opts...)
	}
}

type EQPG struct {
	DB *sql.DB
	nw entroq.NotifyWaiter

	stopReadiness  func()
	readinessDone  chan struct{}
	stopGC         func()
	gcDone         chan struct{}
	claimDuration  metric.Float64Histogram
	modifyDuration metric.Float64Histogram
	gcMetrics      *gcmetrics.Metrics
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

	mp := opts.mp
	if mp == nil {
		mp = noop.NewMeterProvider()
	}
	if err := b.initMetrics(mp); err != nil {
		return nil, fmt.Errorf("eqpg init metrics: %w", err)
	}

	// Background loops start last, after every fallible step, so an error return
	// from New can never leak a goroutine: New either starts nothing and returns
	// an error, or starts everything and returns a backend whose Close stops them.
	//
	// Each loop's context is rooted at context.Background(), NOT the constructor's
	// ctx. Its lifetime is the backend's, ended by Close; the constructor ctx
	// scopes only construction, and callers idiomatically bound New with a timeout
	// and defer cancel(), so deriving from ctx would silently stop the loop the
	// instant New returned while the backend kept serving. Close cancels each
	// loop and waits for it to exit before closing the DB, so it never touches a
	// closed connection and never outlives the backend.
	//
	// The readiness loop needs to know who is waiting; a NotifyWaiter that cannot
	// say leaves claim polling as the only wakeup for tasks that become ready
	// over time.
	if lc, ok := b.nw.(entroq.ListenerCounter); ok && opts.readinessInterval > 0 {
		readinessCtx, stop := context.WithCancel(context.Background())
		b.stopReadiness = stop
		b.readinessDone = make(chan struct{})
		go func() {
			defer close(b.readinessDone)
			b.runReadinessLoop(readinessCtx, lc, opts.readinessInterval)
		}()
	}

	if opts.gcInterval > 0 {
		gcCtx, stop := context.WithCancel(context.Background())
		b.stopGC = stop
		b.gcDone = make(chan struct{})
		go func() {
			defer close(b.gcDone)
			b.runGCLoop(gcCtx, opts.gcInterval, opts.gcBatchSize)
		}()
	}

	return b, nil
}

func (b *EQPG) initMetrics(mp metric.MeterProvider) error {
	meter := mp.Meter("entroq.pg")
	var err error
	b.claimDuration, err = meter.Float64Histogram("entroq.claim.duration",
		metric.WithDescription("Duration of TryClaim calls against the database."),
		metric.WithUnit("s"),
		latency.Buckets(),
	)
	if err != nil {
		return fmt.Errorf("claim duration histogram: %w", err)
	}
	b.modifyDuration, err = meter.Float64Histogram("entroq.modify.duration",
		metric.WithDescription("Duration of Modify calls against the database."),
		metric.WithUnit("s"),
		latency.Buckets(),
	)
	if err != nil {
		return fmt.Errorf("modify duration histogram: %w", err)
	}
	if b.gcMetrics, err = gcmetrics.New(meter); err != nil {
		return fmt.Errorf("gc metrics: %w", err)
	}
	return nil
}

// Close closes the underlying database connection.
func (b *EQPG) Close() error {
	if b.stopReadiness != nil {
		b.stopReadiness()
		<-b.readinessDone // wait for the readiness loop to exit before closing the DB
	}
	if b.stopGC != nil {
		b.stopGC()
		<-b.gcDone // wait for the GC loop to exit before closing the DB
	}
	if err := b.DB.Close(); err != nil {
		return fmt.Errorf("pg backend close: %w", err)
	}
	return nil
}

// pgInterval formats a duration as a Postgres interval literal, split into
// seconds and microseconds so that no single interval field integer exceeds
// int32, which Postgres rejects with SQLSTATE 22015. A bare microseconds field
// overflows at ~35.8 minutes (INT32_MAX microseconds); expressing whole seconds
// separately pushes that ceiling out to ~68 years while preserving full
// microsecond precision.
func pgInterval(d time.Duration) string {
	secs := d / time.Second
	usec := (d % time.Second) / time.Microsecond
	return fmt.Sprintf("%d seconds %d microseconds", secs, usec)
}

// Queues returns the queues and their sizes.
func (b *EQPG) Queues(ctx context.Context, qq *entroq.QueuesQuery) (map[string]int, error) {
	return entroq.QueuesFromStats(b.QueueStats(ctx, qq))
}

// QueueStats returns a mapping from queue names to their statistics.
func (b *EQPG) QueueStats(ctx context.Context, qq *entroq.QueuesQuery) (map[string]*entroq.QueueStat, error) {
	// All metrics come from one index-only grouped scan: a single pass grouped by
	// queue over a covering index on (queue, at, claims), with FILTERs deriving
	// the claimed/future/available counts and MAX(claims) folded into the same
	// aggregate. Read-only and non-blocking -- no heap access -- as long as that
	// covering index exists; without it this degrades to a full heap scan.
	q := `SELECT
			queue,
			COUNT(*) AS count,
			COUNT(*) FILTER(WHERE at > NOW() AND claims > 0) AS claimed,
			COUNT(*) FILTER(WHERE at > NOW() AND claims = 0) AS future,
			COUNT(*) FILTER(WHERE at <= NOW()) as available,
			COALESCE(MAX(claims), 0) AS max_claims
		FROM entroq.tasks`
	var values []any

	if len(qq.MatchPrefix) != 0 || len(qq.MatchExact) != 0 {
		q += " WHERE"
	}

	var matchFragments []string
	for _, m := range qq.MatchPrefix {
		matchFragments = append(matchFragments, fmt.Sprintf(" queue LIKE $%d ESCAPE '\\'", len(values)+1))
		values = append(values, likePrefix(m))
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
	if err := tq.Validate(); err != nil {
		return nil, fmt.Errorf("eqpg tasks: %w", err)
	}
	q := "SELECT id, version, queue, at, created, modified, claimant, value, claims, attempt, err FROM tasks WHERE true"
	var values []any

	if tq.Queue != "" {
		q += fmt.Sprintf(" AND queue = $%d", len(values)+1)
		values = append(values, tq.Queue)
	}

	// A claimant filter keeps what that claimant can act on now: tasks anyone
	// could claim, and tasks it holds. A task's claimant is whoever last wrote
	// it, so the claimant alone does not mean it is held.
	if tq.Claimant != "" {
		q += fmt.Sprintf(" AND (at <= NOW() OR claimant = $%d)", len(values)+1)
		values = append(values, tq.Claimant)
	}

	// No order is promised, but tasks come in arrival order, or in the order
	// their IDs were asked for, as the indexes make that cheap.
	if len(tq.IDs) != 0 {
		q += fmt.Sprintf(" AND id = any($%d) ORDER BY array_position($%[1]d, id)", len(values)+1)
		values = append(values, pq.StringArray(tq.IDs))
	} else {
		q += " ORDER BY at, id"
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
	if err := validate.Claim(cq); err != nil {
		return nil, fmt.Errorf("eqpg claim: %w", err)
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
		pq.Array(cq.Queues), cq.Claimant, pgInterval(cq.Duration),
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
// Experimental: it may change or be removed. It predates docs, which now keep
// state that must change atomically with tasks on every backend, including
// through the service; prefer them.
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
	// Reject writes to an empty queue/namespace before touching the database, so
	// an empty queue is never written.
	if err := validate.Modification(mod); err != nil {
		return nil, fmt.Errorf("eqpg modify: %w", err)
	}
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
	for i := range 7 {
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
		backoff := min(time.Duration(float64((1<<i)*minBackoff)*rand.Float64()), time.Second)
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
func (b *EQPG) modify(ctx context.Context, mod *entroq.Modification, options *modifyConfig) (resp *entroq.ModifyResponse, err error) {
	// Build parallel arrays for task operation set.
	depIDs, depVers, depQueues := taskIDArrays(mod.Depends)
	delIDs, delVers, delQueues := taskIDArrays(mod.Deletes)
	insIDs, insQueues, insAts, insValues, insAttempts, insErrs := insertArrays(mod.Inserts)
	chgIDs, chgVers, chgFromQueues, chgQueues, chgAts, chgValues, chgAttempts, chgErrs := changeArrays(mod.Changes)

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
				resp, err = nil, fmt.Errorf("pg modify commit failed: %w", cmErr)
			}
		}
	}()

	// Run caller's DB work first, inside the same transaction, if specified.
	if options.runInTx != nil {
		if err := options.runInTx(ctx, tx); err != nil {
			return nil, fmt.Errorf("pg modify caller tx work: %w", err)
		}
	}

	resp = new(entroq.ModifyResponse)

	// Doc modifications, by the rules in docgroup.
	if err := modifyDocs(ctx, tx, mod, resp); err != nil {
		return nil, err
	}

	// Task modifications.
	rows, err := tx.QueryContext(ctx, `
		SELECT kind, id, version, queue, at, created, modified, claimant, value, claims, attempt, err
		FROM _modify_arrays(
			$1,
			$2::text[], $3::integer[], $4::text[],
			$5::text[], $6::integer[], $7::text[],
			$8::text[], $9::text[], $10::timestamptz[], $11::text[], $12::integer[], $13::text[],
			$14::text[], $15::integer[], $16::text[], $17::text[], $18::timestamptz[], $19::text[], $20::integer[], $21::text[]
		)`,
		mod.Claimant,
		pq.Array(depIDs), pq.Array(depVers), pq.Array(depQueues),
		pq.Array(delIDs), pq.Array(delVers), pq.Array(delQueues),
		pq.Array(insIDs), pq.Array(insQueues), pq.Array(insAts), pq.Array(insValues), pq.Array(insAttempts), pq.Array(insErrs),
		pq.Array(chgIDs), pq.Array(chgVers), pq.Array(chgFromQueues), pq.Array(chgQueues), pq.Array(chgAts), pq.Array(chgValues), pq.Array(chgAttempts), pq.Array(chgErrs),
	)
	if err != nil {
		return nil, parseModifyError(err, mod)
	}
	defer rows.Close()

	for rows.Next() {
		t := new(entroq.Task)
		var kind string
		var val []byte
		if err := rows.Scan(&kind, &t.ID, &t.Version, &t.Queue, &t.At, &t.Created, &t.Modified, &t.Claimant, &val, &t.Claims, &t.Attempt, &t.Err); err != nil {
			return nil, fmt.Errorf("pg modify task scan: %w", err)
		}
		t.Value = val
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
		Claimed []struct {
			ID      string `json:"id"`
			Version int32  `json:"version"`
		} `json:"claimed"`
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
	for _, c := range detail.Claimed {
		depErr.Claims = append(depErr.Claims, &entroq.TaskID{ID: c.ID, Version: c.Version})
	}
	for _, c := range detail.Collisions {
		depErr.Inserts = append(depErr.Inserts, &entroq.TaskID{ID: c.ID, Version: c.Version})
	}

	return depErr
}

// taskIDArrays splits a slice of TaskIDs into parallel ID string and version slices.
func taskIDArrays(tids []*entroq.TaskID) (ids []string, versions []int32, queues []string) {
	ids = make([]string, len(tids))
	versions = make([]int32, len(tids))
	queues = make([]string, len(tids))
	for i, t := range tids {
		ids[i] = t.ID
		versions[i] = t.Version
		queues[i] = t.Queue // claimed current queue; part of the modify key
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

// changeArrays splits a slice of Task changes into parallel arrays for the
// stored procedure. fromQueues is the source (current) queue matched by the
// modify key; queues is the destination the task moves to (equal for a plain
// change).
func changeArrays(changes []*entroq.Task) (ids []string, versions []int32, fromQueues []string, queues []string, ats []time.Time, values []*string, attempts []int32, errs []string) {
	ids = make([]string, len(changes))
	versions = make([]int32, len(changes))
	fromQueues = make([]string, len(changes))
	queues = make([]string, len(changes))
	ats = make([]time.Time, len(changes))
	values = make([]*string, len(changes))
	attempts = make([]int32, len(changes))
	errs = make([]string, len(changes))
	for i, chg := range changes {
		ids[i] = chg.ID
		versions[i] = chg.Version
		fromQueues[i] = chg.FromQueue
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
		if err := rows.Scan(&r.Namespace, &r.ID, &r.Version, &r.Claimant, &r.At, &r.Key, &r.SecondaryKey, &val, &r.Created, &r.Modified); err != nil {
			return nil, fmt.Errorf("pg docs scan: %w", err)
		}
		r.Content = val
		results = append(results, r)
	}
	return results, rows.Err()
}

// Docs returns docs in a namespace. If IDs are specified, only those docs are
// returned (key range and limit are ignored). Otherwise, docs are filtered by
// optional key range and subject to limit. Each doc carries its group's
// version and claim.
func (b *EQPG) Docs(ctx context.Context, rq *entroq.DocQuery) ([]*entroq.Doc, error) {
	if err := rq.Validate(); err != nil {
		return nil, fmt.Errorf("eqpg docs: %w", err)
	}
	var (
		rows *sql.Rows
		err  error
	)
	columns := docColumns
	if rq.OmitValues {
		columns = strings.Replace(columns, "d.value", "NULL::jsonb", 1)
	}
	if len(rq.IDs) > 0 {
		rows, err = b.DB.QueryContext(ctx,
			`SELECT `+columns+` FROM `+docsWithLocks+`
			 WHERE d.namespace = $1 AND d.id = ANY($2)
			 ORDER BY array_position($2, d.id)`,
			rq.Namespace, pq.StringArray(rq.IDs),
		)
	} else if rq.KeyExact != "" {
		rows, err = b.DB.QueryContext(ctx,
			`SELECT `+columns+` FROM `+docsWithLocks+`
			 WHERE d.namespace = $1 AND d.key_primary = $2
			 ORDER BY d.key_primary, d.key_secondary, d.id
			 LIMIT NULLIF($3, 0)`,
			rq.Namespace, rq.KeyExact, rq.Limit,
		)
	} else {
		rows, err = b.DB.QueryContext(ctx,
			`SELECT `+columns+` FROM `+docsWithLocks+`
			 WHERE d.namespace = $1
			   AND ($2 = '' OR d.key_primary >= $2)
			   AND ($3 = '' OR d.key_primary < $3)
			 ORDER BY d.key_primary, d.key_secondary, d.id
			 LIMIT NULLIF($4, 0)`,
			rq.Namespace, rq.KeyStart, rq.KeyEnd, rq.Limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("pg docs query: %w", err)
	}
	defer rows.Close()
	return scanDocRows(rows)
}

// ClaimDocs claims the group of docs sharing the given primary key in the
// namespace and returns its members, which may be none: a group can be claimed
// before it has docs. It returns a DependencyError listing the members while
// someone else holds the group.
func (b *EQPG) ClaimDocs(ctx context.Context, cq *entroq.DocClaim) (docs []*entroq.Doc, err error) {
	if err := validate.DocClaim(cq); err != nil {
		return nil, fmt.Errorf("claim docs: %w", err)
	}
	tx, err := b.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("pg claim docs begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
			return
		}
		if cmErr := tx.Commit(); cmErr != nil {
			docs, err = nil, fmt.Errorf("pg claim docs commit: %w", cmErr)
		}
	}()
	return claimDocs(ctx, tx, cq)
}
