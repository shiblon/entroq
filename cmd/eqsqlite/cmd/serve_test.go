package cmd

import (
	"strings"
	"testing"
)

func TestServeRejectsEmptyPath(t *testing.T) {
	oldPath := dbPath
	oldChanged := rootCmd.PersistentFlags().Lookup("path").Changed
	t.Cleanup(func() {
		dbPath = oldPath
		rootCmd.PersistentFlags().Lookup("path").Changed = oldChanged
	})

	dbPath = ""
	rootCmd.PersistentFlags().Lookup("path").Changed = true
	err := serveCmd.RunE(serveCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "empty SQLite database path") {
		t.Fatalf("serve error = %v, want empty path", err)
	}
}

func TestResolveSQLitePathFromEnvironment(t *testing.T) {
	oldPath := dbPath
	oldChanged := rootCmd.PersistentFlags().Lookup("path").Changed
	t.Cleanup(func() {
		dbPath = oldPath
		rootCmd.PersistentFlags().Lookup("path").Changed = oldChanged
	})

	dbPath = "entroq.db"
	rootCmd.PersistentFlags().Lookup("path").Changed = false
	t.Setenv("EQ_SQLITE_PATH", "/tmp/from-env.db")
	resolveSQLiteFlags()
	if dbPath != "/tmp/from-env.db" {
		t.Fatalf("dbPath = %q, want environment value", dbPath)
	}
}
