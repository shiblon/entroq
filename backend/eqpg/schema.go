package eqpg

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
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

// initDB checks the stored schema version and either confirms compatibility or
// initializes a fresh database.
//
// Three cases:
//   - Version matches SchemaVersion: return nil (no SQL needed).
//   - Version exists but mismatches: return a descriptive error; the caller
//     must apply schema.sql and bump meta manually before retrying.
//   - No version stored (fresh database or pre-versioning schema): run the
//     full idempotent schema.sql to initialize.
func (b *EQPG) initDB(ctx context.Context) error {
	var stored string
	err := b.DB.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'schema_version'`,
	).Scan(&stored)

	switch {
	case err == nil && stored == SchemaVersion:
		return nil
	case err == nil:
		return fmt.Errorf("schema version mismatch: database has %q, code expects %q; apply schema.sql to migrate, then: UPDATE meta SET value = %q WHERE key = 'schema_version'", stored, SchemaVersion, SchemaVersion)
	case errors.Is(err, sql.ErrNoRows):
		// meta exists but has no version row -- run schema to insert it.
	default:
		// meta table does not exist (or other error): fresh database.
	}

	if _, err := b.DB.ExecContext(ctx, SchemaSQL); err != nil {
		return fmt.Errorf("initDB: %w", err)
	}
	return nil
}
