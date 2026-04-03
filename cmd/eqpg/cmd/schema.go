package cmd

import (
	"context"
	"fmt"
	"log"

	"github.com/shiblon/entroq/backend/eqpg"
	"github.com/spf13/cobra"
)

var schemaCmd = &cobra.Command{
	Use:   "schema",
	Short: "Manage the EntroQ PostgreSQL schema.",
}

var schemaInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize (or re-initialize) the EntroQ schema in the target database.",
	Long: `Apply the full idempotent EntroQ DDL to the target database.

Safe to run on an already-initialized database: all statements use IF NOT EXISTS
and ON CONFLICT DO NOTHING. The service refuses to start until this has been run.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		resolveDBFlags()
		eq, err := eqpg.Open(context.Background(), dbAddr,
			eqpg.WithDB(dbName),
			eqpg.WithUsername(dbUser),
			eqpg.WithPassword(dbPass),
		)
		if err != nil {
			return fmt.Errorf("schema init: %w", err)
		}
		defer eq.Close()

		if err := eqpg.InitSchema(cmd.Context(), eq.DB); err != nil {
			return fmt.Errorf("schema init: %w", err)
		}
		log.Printf("Schema initialized at version %s.", eqpg.SchemaVersion)
		return nil
	},
}

var schemaVersionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the schema version stored in the target database.",
	RunE: func(cmd *cobra.Command, args []string) error {
		resolveDBFlags()
		eq, err := eqpg.Open(context.Background(), dbAddr,
			eqpg.WithDB(dbName),
			eqpg.WithUsername(dbUser),
			eqpg.WithPassword(dbPass),
		)
		if err != nil {
			return fmt.Errorf("schema version: %w", err)
		}
		defer eq.Close()

		v, err := eqpg.StoredSchemaVersion(cmd.Context(), eq.DB)
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
	schemaCmd.AddCommand(schemaInitCmd, schemaVersionCmd, schemaPrintCmd)
	rootCmd.AddCommand(schemaCmd)
}
