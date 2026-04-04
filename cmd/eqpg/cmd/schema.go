package cmd

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/shiblon/entroq/backend/eqpg"
	"github.com/spf13/cobra"

	_ "github.com/lib/pq"
)

var schemaCmd = &cobra.Command{
	Use:   "schema",
	Short: "Manage the EntroQ PostgreSQL schema.",
}

// openDB opens a *sql.DB via eqpg.OpenDB - no schema version check - for use
// by schema subcommands that need to operate on uninitialized or legacy databases.
func openDB() (*sql.DB, error) {
	resolveDBFlags()
	return eqpg.OpenDB(dbAddr,
		eqpg.WithDB(dbName),
		eqpg.WithUsername(dbUser),
		eqpg.WithPassword(dbPass),
	)
}

var schemaInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize (or re-initialize) the EntroQ schema in the target database.",
	Long: `Apply the full idempotent EntroQ DDL to the target database.

Safe to run on an already-initialized database: all statements use IF NOT EXISTS
and ON CONFLICT DO NOTHING. The service refuses to start until this has been run.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return fmt.Errorf("schema init: %w", err)
		}
		defer db.Close()

		if err := eqpg.InitSchema(cmd.Context(), db); err != nil {
			return fmt.Errorf("schema init: %w", err)
		}
		log.Printf("Schema initialized at version %s.", eqpg.SchemaVersion)
		return nil
	},
}

var schemaUpgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade the database schema to the current version.",
	Long: `Detect the current database state and upgrade it to the current schema version.

Handles two legacy states:
  - public.tasks exists (pre-0.10): moves the table into the entroq schema,
    then applies the full schema DDL (which handles UUID->text conversion,
    new indexes, stored functions, and version tracking).
  - entroq.tasks exists but version is missing or mismatched: re-applies the
    schema DDL.

Safe to run on an already-current database: reports that no upgrade was needed
and exits cleanly.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return fmt.Errorf("schema upgrade: %w", err)
		}
		defer db.Close()

		result, err := eqpg.UpgradeSchema(cmd.Context(), db)
		if err != nil {
			return fmt.Errorf("schema upgrade: %w", err)
		}
		log.Printf("Schema upgrade: %s (version %s).", result, eqpg.SchemaVersion)
		return nil
	},
}

var schemaVersionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the schema version stored in the target database.",
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return fmt.Errorf("schema version: %w", err)
		}
		defer db.Close()

		v, err := eqpg.StoredSchemaVersion(cmd.Context(), db)
		if err != nil {
			return fmt.Errorf("schema version: %w", err)
		}
		fmt.Println(v)
		return nil
	},
}

var schemaPrintCmd = &cobra.Command{
	Use:   "print",
	Short: "Print the bundled schema SQL to stdout (no database connection needed).",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Print(eqpg.SchemaSQL)
	},
}

func init() {
	schemaCmd.AddCommand(schemaInitCmd, schemaUpgradeCmd, schemaVersionCmd, schemaPrintCmd)
	rootCmd.AddCommand(schemaCmd)
}
