package cmd

import (
	"strings"
	"testing"
)

func resetDatabaseFlags(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"PGURL", "PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD",
		"PGSSLMODE", "PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY",
	} {
		t.Setenv(name, "")
	}
	oldURL, oldAddr, oldName, oldUser, oldPass := dbURL, dbAddr, dbName, dbUser, dbPass
	oldSSLMode, oldSSLRootCert, oldSSLCert, oldSSLKey := dbSSLMode, dbSSLRootCert, dbSSLCert, dbSSLKey
	t.Cleanup(func() {
		dbURL, dbAddr, dbName, dbUser, dbPass = oldURL, oldAddr, oldName, oldUser, oldPass
		dbSSLMode, dbSSLRootCert, dbSSLCert, dbSSLKey = oldSSLMode, oldSSLRootCert, oldSSLCert, oldSSLKey
	})
	dbURL, dbAddr, dbName, dbUser, dbPass = "", "", "", "", ""
	dbSSLMode, dbSSLRootCert, dbSSLCert, dbSSLKey = "", "", "", ""
}

func TestDatabaseConnectionURLPrecedence(t *testing.T) {
	resetDatabaseFlags(t)
	t.Setenv("PGURL", "postgresql://env:env-secret@env.example/envdb")
	t.Setenv("PGHOST", "ignored.example")
	dbURL = "postgresql://flag:flag-secret@flag.example/flagdb?sslmode=require"

	target, options := databaseConnection()
	if target != dbURL {
		t.Fatalf("database target = %q, want explicit URL %q", target, dbURL)
	}
	if len(options) != 0 {
		t.Fatalf("URL connection received %d decomposed options, want none", len(options))
	}
	description := databaseDescription(target)
	if strings.Contains(description, "secret") {
		t.Fatalf("database description exposed credentials: %q", description)
	}
	if description != "postgres(flag.example db=flagdb user=flag)" {
		t.Fatalf("database description = %q", description)
	}
}

func TestDatabaseConnectionFromPGURL(t *testing.T) {
	resetDatabaseFlags(t)
	t.Setenv("PGURL", "postgresql://aso:aso@database.example:5432/aso?sslmode=require")
	t.Setenv("PGHOST", "ignored.example")

	target, options := databaseConnection()
	if target != "postgresql://aso:aso@database.example:5432/aso?sslmode=require" {
		t.Fatalf("database target = %q", target)
	}
	if len(options) != 0 {
		t.Fatalf("PGURL connection received %d decomposed options, want none", len(options))
	}
}

func TestDatabaseConnectionFromDecomposedEnvironment(t *testing.T) {
	resetDatabaseFlags(t)
	t.Setenv("PGURL", "")
	t.Setenv("PGHOST", "database.example")
	t.Setenv("PGPORT", "5544")
	t.Setenv("PGDATABASE", "entroq")
	t.Setenv("PGUSER", "worker")
	t.Setenv("PGPASSWORD", "secret")

	target, options := databaseConnection()
	if target != "database.example:5544" {
		t.Fatalf("database target = %q, want database.example:5544", target)
	}
	if len(options) != 3 {
		t.Fatalf("decomposed connection received %d options, want 3", len(options))
	}
	if got := databaseDescription(target); got != "postgres(database.example:5544 db=entroq user=worker)" {
		t.Fatalf("database description = %q", got)
	}
}

func TestDatabaseConnectionFromDecomposedTLSEnvironment(t *testing.T) {
	resetDatabaseFlags(t)
	t.Setenv("PGURL", "")
	t.Setenv("PGSSLMODE", "verify-full")
	t.Setenv("PGSSLROOTCERT", "/certs/root.pem")
	t.Setenv("PGSSLCERT", "/certs/client.pem")
	t.Setenv("PGSSLKEY", "/certs/client-key.pem")

	_, options := databaseConnection()
	if len(options) != 6 {
		t.Fatalf("decomposed TLS connection received %d options, want 6", len(options))
	}
}

func TestDatabaseConnectionDefaultAddress(t *testing.T) {
	resetDatabaseFlags(t)
	t.Setenv("PGURL", "")
	t.Setenv("PGHOST", "")
	t.Setenv("PGPORT", "")

	target, _ := databaseConnection()
	if target != ":5432" {
		t.Fatalf("database target = %q, want :5432", target)
	}
}
