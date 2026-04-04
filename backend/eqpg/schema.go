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
const SchemaVersion = "0.10.0"

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
