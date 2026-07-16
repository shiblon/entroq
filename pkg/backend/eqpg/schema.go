package eqpg

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
)

// SchemaSQL is the full idempotent DDL for the EntroQ PostgreSQL schema,
// suitable for running directly with psql or any PostgreSQL client.
//
//go:embed schema.sql
var SchemaSQL string

// SchemaVersion is the schema version this build of eqpg expects to find in the
// database. It is the module version in which the schema last changed; initDB
// refuses to open the backend if the stored version differs, protecting against
// accidental use of a mismatched schema.
//
// Invariant: the schema version never exceeds the module version. The module may
// advance without the schema (not every change touches it), but a schema version
// ahead of the module implies code expecting a release that does not exist. This
// is a release-time property -- during development the schema is legitimately
// ahead of the last tag, since it is prepared for the upcoming release -- so the
// release process, not a unit test, must enforce that the tag being cut is >=
// SchemaVersion. (TestSchemaFilesInSync guards the related lockstep: it requires
// this constant, the version schema.sql stamps into entroq.meta, and the Python
// client's SCHEMA_VERSION to all agree, and the two schema.sql copies to be
// byte-identical.)
//
// Versioning policy (1.x+):
//   - The schema version changes only when the schema itself changes, and takes
//     the version of the release that ships the change.
//   - Minor releases (1.x -> 1.y) may change the schema. Additive changes (new
//     tables, columns with defaults, indexes, functions) are always fine. A
//     non-additive change (a type change or data movement) is permitted only
//     when it is transparent to clients and re-applying schema.sql performs it
//     idempotently -- e.g. the 1.7.1 byte-order collation change, which alters
//     only lexicographical ordering, not data or the client contract. Such a
//     migration may rewrite tables and rebuild indexes, so it needs a
//     maintenance window (see the changelog and UpgradeSchema).
//   - Patch releases never change the schema.
//   - Any 1.x schema upgrades to any later 1.y by re-running schema.sql.
//   - A schema version is skipped when a value was used transiently for a
//     different schema on an unreleased branch, to keep re-run detection sound:
//     1.7.0 is skipped because it named a queue-array-only schema on develop
//     that lacked the 1.7.1 collation change.
//   - Schemas predating 1.0 cannot be migrated; see UpgradeSchema.
const SchemaVersion = "1.7.1"

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
	UpgradeApplied                             // schema was re-applied (new minor version)
)

func (r UpgradeResult) String() string {
	switch r {
	case UpgradeAlreadyCurrent:
		return "already current"
	case UpgradeApplied:
		return "schema applied"
	default:
		return "unknown"
	}
}

// UpgradeSchema brings the database to SchemaVersion:
//
//   - Already at SchemaVersion: returns UpgradeAlreadyCurrent; nothing is changed.
//   - 1.x schema at an older minor version: re-applies schema.sql idempotently
//     and returns UpgradeApplied. Most upgrades are additive, but a minor may
//     include a client-transparent non-additive migration (see SchemaVersion),
//     which can rewrite tables and rebuild indexes -- plan a maintenance window.
//   - Pre-1.0 schema (0.x): returns an error. The 0.x -> 1.0 gap is too large
//     to migrate automatically. Drain all tasks, drop the schema, and
//     reinitialize: DROP SCHEMA entroq CASCADE, then run eqpg schema init.
//   - Uninitialized database: applies schema.sql fresh and returns UpgradeApplied.
func UpgradeSchema(ctx context.Context, db *sql.DB) (UpgradeResult, error) {
	stored, err := StoredSchemaVersion(ctx, db)
	if err == nil {
		if stored == SchemaVersion {
			return UpgradeAlreadyCurrent, nil
		}
		if strings.HasPrefix(stored, "0.") {
			return 0, fmt.Errorf(
				"schema version %q predates 1.0 and cannot be migrated automatically; "+
					"drain all tasks and reinitialize: DROP SCHEMA entroq CASCADE, "+
					"then run: eqpg schema init",
				stored,
			)
		}
	}

	if err := InitSchema(ctx, db); err != nil {
		return 0, fmt.Errorf("upgrade: apply schema: %w", err)
	}
	return UpgradeApplied, nil
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
