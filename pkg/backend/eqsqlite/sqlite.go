// Package eqsqlite implements a SQLite-backed entroq.Backend.
//
// The package is intentionally EXPERIMENTAL: its API, schema, and on-disk
// format may change or be removed without a migration path.
package eqsqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/internal/gcmetrics"
	"github.com/shiblon/entroq/pkg/subq"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	_ "modernc.org/sqlite"
)

// SchemaVersion is the current database schema version.
const SchemaVersion = 3

//go:embed schema.sql
var schemaSQL string

type options struct {
	nw          entroq.NotifyWaiter
	mp          metric.MeterProvider
	gcInterval  time.Duration
	gcBatchSize int
	busyTimeout time.Duration

	readinessInterval time.Duration
}

// Option configures the SQLite backend.
type Option func(*options)

// WithNotifyWaiter replaces the in-process notification queue used by Claim.
func WithNotifyWaiter(nw entroq.NotifyWaiter) Option {
	return func(o *options) { o.nw = nw }
}

// WithMeterProvider sets the OpenTelemetry provider used for backend metrics.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.mp = mp }
}

// EQSQLite is a persistent SQLite implementation of entroq.Backend.
type EQSQLite struct {
	readDB  *sql.DB
	writeDB *sql.DB
	nw      entroq.NotifyWaiter

	stopGC    context.CancelFunc
	gcDone    chan struct{}
	gcMetrics *gcmetrics.Metrics

	stopReadiness context.CancelFunc
	readinessDone chan struct{}
	claimDur      metric.Float64Histogram
	modifyDur     metric.Float64Histogram

	closeOnce sync.Once
	closeErr  error
}

var _ entroq.Backend = (*EQSQLite)(nil)

// Opener returns a SQLite BackendOpener for path.
func Opener(path string, opts ...Option) entroq.BackendOpener {
	return func(ctx context.Context) (entroq.Backend, error) {
		return Open(ctx, path, opts...)
	}
}

// Open opens or creates a SQLite backend at path.
func Open(ctx context.Context, path string, opts ...Option) (*EQSQLite, error) {
	if path == "" {
		return nil, fmt.Errorf("eqsqlite open: empty database path")
	}
	o := options{
		gcInterval:  defaultGCInterval,
		gcBatchSize: defaultGCBatchSize,
		busyTimeout: 5 * time.Second,

		readinessInterval: DefaultReadinessInterval,
	}
	for _, opt := range opts {
		opt(&o)
	}

	nw := o.nw
	if nw == nil {
		nw = subq.New()
	}
	mp := o.mp
	if mp == nil {
		mp = noop.NewMeterProvider()
	}
	gcMetrics, err := gcmetrics.New(mp.Meter("entroq.sqlite"))
	if err != nil {
		return nil, fmt.Errorf("eqsqlite open: gc metrics: %w", err)
	}
	meter := mp.Meter("entroq.sqlite")
	claimDur, err := meter.Float64Histogram("entroq.claim.duration",
		metric.WithDescription("Duration of TryClaim calls against SQLite."),
		metric.WithUnit("s"))
	if err != nil {
		return nil, fmt.Errorf("eqsqlite open: claim metrics: %w", err)
	}
	modifyDur, err := meter.Float64Histogram("entroq.modify.duration",
		metric.WithDescription("Duration of Modify calls against SQLite."),
		metric.WithUnit("s"))
	if err != nil {
		return nil, fmt.Errorf("eqsqlite open: modify metrics: %w", err)
	}

	writeDB, readDB, err := openDatabases(ctx, path, o.busyTimeout)
	if err != nil {
		return nil, fmt.Errorf("eqsqlite open: %w", err)
	}

	gcCtx, stopGC := context.WithCancel(context.Background())
	b := &EQSQLite{
		readDB:    readDB,
		writeDB:   writeDB,
		nw:        nw,
		stopGC:    stopGC,
		gcDone:    make(chan struct{}),
		gcMetrics: gcMetrics,
		claimDur:  claimDur,
		modifyDur: modifyDur,
	}
	go func() {
		defer close(b.gcDone)
		b.runGCLoop(gcCtx, o.gcInterval, o.gcBatchSize)
	}()
	// Like GC, the readiness loop lives as long as the backend. It needs to
	// know who is waiting; a custom NotifyWaiter that cannot say leaves claim
	// polling as the only wakeup for tasks that become ready over time.
	if lc, ok := nw.(entroq.ListenerCounter); ok && o.readinessInterval > 0 {
		readinessCtx, stop := context.WithCancel(context.Background())
		b.stopReadiness = stop
		b.readinessDone = make(chan struct{})
		go func() {
			defer close(b.readinessDone)
			b.runReadinessLoop(readinessCtx, lc, o.readinessInterval)
		}()
	}

	return b, nil
}

func openDatabases(ctx context.Context, path string, timeout time.Duration) (_ *sql.DB, _ *sql.DB, err error) {
	writeDB, err := openWriteDB(ctx, path, timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("open write: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, writeDB.Close())
		}
	}()

	readDB, err := openReadDB(ctx, path, timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("open read: %w", err)
	}
	return writeDB, readDB, nil
}

func openWriteDB(ctx context.Context, path string, timeout time.Duration) (_ *sql.DB, err error) {
	db, err := sql.Open("sqlite", sqliteDSN(path, timeout, false))
	if err != nil {
		return nil, fmt.Errorf("writer: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, db.Close())
		}
	}()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("writer connection: %w", err)
	}
	defer func() { err = errors.Join(err, conn.Close()) }()

	var journalMode string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if journalMode != "wal" {
		return nil, fmt.Errorf("requested WAL, got %q", journalMode)
	}
	// A database without the meta table is new; everything else was created
	// by some earlier build, and its version and digest say which.
	var tables int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'entroq_meta'").Scan(&tables); err != nil {
		return nil, fmt.Errorf("find schema: %w", err)
	}
	fresh := tables == 0
	if _, err := conn.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	var schemaVersion int
	if err := conn.QueryRowContext(ctx, "SELECT schema_version FROM entroq_meta WHERE id = 1").Scan(&schemaVersion); err != nil {
		return nil, fmt.Errorf("schema version: %w", err)
	}
	switch {
	case fresh:
		if _, err := conn.ExecContext(ctx, "UPDATE entroq_meta SET schema_digest = ? WHERE id = 1", schemaDigest); err != nil {
			return nil, fmt.Errorf("stamp schema digest: %w", err)
		}
	case schemaVersion == 1 || schemaVersion == 2:
		if err := migrateToV3(ctx, conn, schemaVersion); err != nil {
			return nil, fmt.Errorf("migrate schema %d to 3: %w", schemaVersion, err)
		}
	case schemaVersion != SchemaVersion:
		return nil, fmt.Errorf("schema version %d, backend requires %d", schemaVersion, SchemaVersion)
	}
	digest, err := storedDigest(ctx, conn)
	if err != nil {
		return nil, err
	}
	if digest != schemaDigest {
		return nil, fmt.Errorf("schema version %d was created by a development build with a different layout; "+
			"move the file aside and let this build create a new one", schemaVersion)
	}
	return db, nil
}

// schemaDigest identifies the exact schema this build creates. Version 3
// changed on unreleased development builds, so a version alone cannot tell
// those layouts apart; a file whose digest differs is refused rather than
// run on the wrong layout.
var schemaDigest = func() string {
	sum := sha256.Sum256([]byte(schemaSQL))
	return hex.EncodeToString(sum[:])
}()

// storedDigest reads the recorded schema digest, or "" for a database created
// before digests were recorded.
func storedDigest(ctx context.Context, conn *sql.Conn) (string, error) {
	var columns int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('entroq_meta') WHERE name = 'schema_digest'").Scan(&columns); err != nil {
		return "", fmt.Errorf("find schema digest: %w", err)
	}
	if columns == 0 {
		return "", nil
	}
	var digest string
	if err := conn.QueryRowContext(ctx, "SELECT schema_digest FROM entroq_meta WHERE id = 1").Scan(&digest); err != nil {
		return "", fmt.Errorf("schema digest: %w", err)
	}
	return digest, nil
}

// migrateToV3 rebuilds a version 1 or 2 database in the version 3 layout, in
// one transaction; any failure leaves the file as it was.
//
// Version 2 made the length CHECKs count bytes, which SQLite cannot alter in
// place, so a version 1 database rebuilds tasks too. Version 3 moves each
// doc's version, claimant, and arrival time to its group's lock: every group
// gets a lock one version past its highest member, so no version read before
// the migration can match one written after it, and claims held at migration
// time are released. The old tables are renamed aside, schemaSQL creates the
// new ones, and the rows are copied across. A row that violates the new
// limits fails its copy and rolls back the whole migration.
func migrateToV3(ctx context.Context, conn *sql.Conn, from int) (err error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	type step struct{ name, sql string }
	var steps []step
	if from == 1 {
		steps = append(steps,
			step{"set aside tasks", "ALTER TABLE tasks RENAME TO tasks_old; DROP INDEX tasks_queue_at"})
	}
	steps = append(steps,
		step{"set aside docs", "ALTER TABLE docs RENAME TO docs_old; DROP INDEX docs_namespace_keys; DROP INDEX IF EXISTS docs_namespace_at"},
		step{"create tables", schemaSQL + "; ALTER TABLE entroq_meta ADD COLUMN schema_digest TEXT NOT NULL DEFAULT ''"})
	if from == 1 {
		steps = append(steps,
			step{"copy tasks (an id or claimant may exceed 64 bytes)", "INSERT INTO tasks SELECT * FROM tasks_old"},
			step{"drop old tasks", "DROP TABLE tasks_old"})
	}
	steps = append(steps,
		step{"create doc group locks", fmt.Sprintf(`INSERT INTO doc_locks (namespace, key_primary, version, claimant, at_ms)
			SELECT namespace, key_primary, max(version) + 1, '', %d FROM docs_old
			GROUP BY namespace, key_primary`, nowUTC().UnixMilli())},
		step{"copy docs (an id or key may exceed its byte limit)", `INSERT INTO docs
			(namespace, id, key_primary, key_secondary, content, created_ms, modified_ms)
			SELECT namespace, id, key_primary, key_secondary, content, created_ms, modified_ms FROM docs_old`},
		step{"drop old docs", "DROP TABLE docs_old"},
		step{"stamp version", fmt.Sprintf("UPDATE entroq_meta SET schema_version = 3, schema_digest = '%s' WHERE id = 1", schemaDigest)})
	for _, st := range steps {
		if _, err := tx.ExecContext(ctx, st.sql); err != nil {
			return fmt.Errorf("%s: %w", st.name, err)
		}
	}
	return tx.Commit()
}

func openReadDB(ctx context.Context, path string, timeout time.Duration) (_ *sql.DB, err error) {
	db, err := sql.Open("sqlite", sqliteDSN(path, timeout, true))
	if err != nil {
		return nil, fmt.Errorf("readers: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, db.Close())
		}
	}()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("reader ping: %w", err)
	}
	return db, nil
}

func sqliteDSN(path string, timeout time.Duration, queryOnly bool) string {
	u := &url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", timeout.Milliseconds()))
	q.Add("_pragma", "foreign_keys(1)")
	if queryOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "synchronous(FULL)")
		q.Add("_txlock", "immediate")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (b *EQSQLite) write(ctx context.Context, call func(context.Context, *sql.Tx) (any, error)) (value any, err error) {
	tx, err := b.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("write tx: %w", err)
	}
	defer tx.Rollback()
	value, err = call(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("write call: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("write commit: %w", err)
	}
	return value, nil
}

// Close stops background work and closes all SQLite connections.
func (b *EQSQLite) Close() error {
	b.closeOnce.Do(func() {
		b.stopGC()
		<-b.gcDone
		if b.stopReadiness != nil {
			b.stopReadiness()
			<-b.readinessDone
		}
		if err := b.writeDB.Close(); err != nil {
			b.closeErr = err
		}
		if err := b.readDB.Close(); err != nil && b.closeErr == nil {
			b.closeErr = err
		}
	})
	if b.closeErr != nil {
		return fmt.Errorf("eqsqlite close: %w", b.closeErr)
	}
	return nil
}

// Time returns the host wall clock in UTC at SQLite's millisecond precision.
func (b *EQSQLite) Time(context.Context) (time.Time, error) {
	return nowUTC(), nil
}

func nowUTC() time.Time {
	return time.UnixMilli(time.Now().UTC().UnixMilli()).UTC()
}
