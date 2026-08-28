// Package eqsqlite implements a SQLite-backed entroq.Backend.
//
// The package is intentionally EXPERIMENTAL: its API, schema, and on-disk
// format may change or be removed without a migration path.
package eqsqlite

import (
	"context"
	"database/sql"
	_ "embed"
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
const SchemaVersion = 1

//go:embed schema.sql
var schemaSQL string

type options struct {
	nw          entroq.NotifyWaiter
	mp          metric.MeterProvider
	gcInterval  time.Duration
	gcBatchSize int
	busyTimeout time.Duration
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
	claimDur  metric.Float64Histogram
	modifyDur metric.Float64Histogram

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
	if _, err := conn.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	var schemaVersion int
	if err := conn.QueryRowContext(ctx, "SELECT schema_version FROM entroq_meta WHERE id = 1").Scan(&schemaVersion); err != nil {
		return nil, fmt.Errorf("schema version: %w", err)
	}
	if schemaVersion != SchemaVersion {
		return nil, fmt.Errorf("schema version %d, backend requires %d", schemaVersion, SchemaVersion)
	}
	return db, nil
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
