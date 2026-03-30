package eqpg

import (
	"context"
	_ "embed"
	"fmt"
)

// SchemaSQL is the full idempotent DDL for the EntroQ PostgreSQL schema,
// suitable for running directly with psql or any PostgreSQL client.
//
//go:embed schema.sql
var SchemaSQL string

// initDB sets up the database to have the appropriate tables and necessary
// extensions to work as a task queue backend. All steps are idempotent.
func (b *backend) initDB(ctx context.Context) error {
	if _, err := b.db.ExecContext(ctx, SchemaSQL); err != nil {
		return fmt.Errorf("initDB: %w", err)
	}
	return nil
}
