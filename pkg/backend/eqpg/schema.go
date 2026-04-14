package eqpg

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

// SchemaSQL is the full idempotent DDL for the EntroQ PostgreSQL schema,
// suitable for running directly with psql or any PostgreSQL client.
//
//go:embed schema.sql
var SchemaSQL string

// SchemaVersion is the schema version this build of eqpg expects to find in
// the database. initDB refuses to open the backend if the stored version
// differs, protecting against accidental use of a mismatched schema.
const SchemaVersion = "0.12.0"

// InitSchema applies the full idempotent EntroQ DDL to db. Safe to run on an
// already-initialized database.
func InitSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, SchemaSQL); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	return nil
}

// StoredSchemaVersion returns the schema_version value recorded in entroq.meta.
// Returns an error if the table or row does not exist (database not initialized).
func StoredSchemaVersion(ctx context.Context, db *sql.DB) (string, error) {
	var v string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'schema_version'`,
	).Scan(&v)
	if err != nil {
		return "", fmt.Errorf("stored schema version: %w", err)
	}
	return v, nil
}

// InitSchema applies the full idempotent EntroQ DDL to this backend's database.
// Convenience wrapper around the package-level InitSchema.
func (b *EQPG) InitSchema(ctx context.Context) error {
	return InitSchema(ctx, b.DB)
}

// UpgradeResult describes what UpgradeSchema did.
type UpgradeResult int

const (
	UpgradeAlreadyCurrent UpgradeResult = iota // schema was already at SchemaVersion; nothing done
	UpgradeFromLegacy                          // public.tasks moved to entroq.tasks and schema applied
	UpgradeApplied                             // entroq.tasks existed but schema was re-applied
)

func (r UpgradeResult) String() string {
	switch r {
	case UpgradeAlreadyCurrent:
		return "already current"
	case UpgradeFromLegacy:
		return "upgraded from legacy (public.tasks -> entroq.tasks)"
	case UpgradeApplied:
		return "schema applied"
	default:
		return "unknown"
	}
}

// tableExists reports whether a table exists in the given schema.
func tableExists(ctx context.Context, db *sql.DB, schema, table string) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = $1 AND table_name = $2
		)`, schema, table,
	).Scan(&exists)
	return exists, err
}

// UpgradeSchema brings the database to SchemaVersion, detecting its current
// state and taking the appropriate action:
//
//   - Already at SchemaVersion: returns UpgradeAlreadyCurrent; nothing is changed.
//   - Legacy state (public.tasks exists, no entroq schema): moves public.tasks
//     into the entroq schema, then applies schema.sql (which handles the
//     UUID->text cast, constraints, indexes, functions, and meta version).
//   - entroq.tasks exists but schema version is missing or mismatched: applies
//     schema.sql to bring it up to date.
//
// Safe to run on an already-current database.
func UpgradeSchema(ctx context.Context, db *sql.DB) (UpgradeResult, error) {
	// Already current?
	if v, err := StoredSchemaVersion(ctx, db); err == nil && v == SchemaVersion {
		return UpgradeAlreadyCurrent, nil
	}

	// Legacy state: public.tasks exists without entroq.tasks.
	publicExists, err := tableExists(ctx, db, "public", "tasks")
	if err != nil {
		return 0, fmt.Errorf("upgrade: check public.tasks: %w", err)
	}
	entroqExists, err := tableExists(ctx, db, "entroq", "tasks")
	if err != nil {
		return 0, fmt.Errorf("upgrade: check entroq.tasks: %w", err)
	}

	result := UpgradeApplied
	if publicExists && !entroqExists {
		if _, err := db.ExecContext(ctx,
			`CREATE SCHEMA IF NOT EXISTS entroq;
			 ALTER TABLE public.tasks SET SCHEMA entroq;`,
		); err != nil {
			return 0, fmt.Errorf("upgrade: move public.tasks to entroq: %w", err)
		}
		result = UpgradeFromLegacy
	}

	if err := InitSchema(ctx, db); err != nil {
		return 0, fmt.Errorf("upgrade: apply schema: %w", err)
	}
	return result, nil
}

// UpgradeSchema brings this backend's database to SchemaVersion.
// Convenience wrapper around the package-level UpgradeSchema.
func (b *EQPG) UpgradeSchema(ctx context.Context) (UpgradeResult, error) {
	return UpgradeSchema(ctx, b.DB)
}

// initDB checks the stored schema version and either confirms compatibility or
// returns an actionable error.
//
// Three cases:
//   - Version matches SchemaVersion: return nil.
//   - Version exists but mismatches: return a descriptive error directing the
//     operator to run "eqpg schema init".
//   - No version row / meta table missing: return an error directing the
//     operator to run "eqpg schema init" first.
func (b *EQPG) initDB(ctx context.Context) error {
	stored, err := StoredSchemaVersion(ctx, b.DB)
	switch {
	case err == nil && stored == SchemaVersion:
		return nil
	case err == nil:
		return fmt.Errorf("schema version mismatch: database has %q, code expects %q; run: eqpg schema init", stored, SchemaVersion)
	default:
		return fmt.Errorf("schema not initialized (run: eqpg schema init): %w", err)
	}
}
