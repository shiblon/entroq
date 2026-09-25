package eqsqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shiblon/entroq"
)

// schemaV1 is the version 1 table layout, whose length CHECKs counted
// characters, kept to exercise the migration to version 2.
const schemaV1 = `
CREATE TABLE entroq_meta (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    schema_version INTEGER NOT NULL
);
INSERT INTO entroq_meta (id, schema_version) VALUES (1, 1);
CREATE TABLE tasks (
    id          TEXT PRIMARY KEY COLLATE BINARY,
    version     INTEGER NOT NULL,
    queue       TEXT NOT NULL COLLATE BINARY CHECK (queue <> ''),
    at_ms       INTEGER NOT NULL,
    claimant    TEXT NOT NULL COLLATE BINARY,
    claims      INTEGER NOT NULL,
    value       TEXT CHECK (value IS NULL OR json_valid(value)),
    created_ms  INTEGER NOT NULL,
    modified_ms INTEGER NOT NULL,
    attempt     INTEGER NOT NULL,
    err         TEXT NOT NULL,
    CHECK (length(id) <= 64),
    CHECK (length(claimant) <= 64)
);
CREATE INDEX tasks_queue_at ON tasks (queue, at_ms, id);
CREATE TABLE docs (
    namespace     TEXT NOT NULL COLLATE BINARY CHECK (namespace <> ''),
    id            TEXT NOT NULL COLLATE BINARY,
    version       INTEGER NOT NULL,
    claimant      TEXT NOT NULL COLLATE BINARY,
    at_ms         INTEGER NOT NULL,
    key_primary   TEXT NOT NULL COLLATE BINARY,
    key_secondary TEXT NOT NULL COLLATE BINARY,
    content       TEXT CHECK (content IS NULL OR json_valid(content)),
    created_ms    INTEGER NOT NULL,
    modified_ms   INTEGER NOT NULL,
    PRIMARY KEY (namespace, id),
    CHECK (length(namespace) <= 64),
    CHECK (length(id) <= 64),
    CHECK (length(claimant) <= 64),
    CHECK (length(key_primary) <= 256),
    CHECK (length(key_secondary) <= 256)
);
CREATE INDEX docs_namespace_keys ON docs (namespace, key_primary, key_secondary, id);
CREATE INDEX docs_namespace_at ON docs (namespace, at_ms, id);
`

func execRaw(ctx context.Context, t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(path, 5*time.Second, false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("exec %.60q: %v", stmt, err)
		}
	}
}

func rawSchemaVersion(ctx context.Context, t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(path, 5*time.Second, true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRowContext(ctx, "SELECT schema_version FROM entroq_meta WHERE id = 1").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// checkSchemaRejects writes over-limit values straight into the tasks and docs
// tables at path, bypassing the Go checks, and fails for each one the schema
// CHECKs accept.
func checkSchemaRejects(ctx context.Context, t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(path, 5*time.Second, false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// wide is n characters and 2n bytes; nul is one counted character before a
	// NUL, n bytes in total.
	wide := func(n int) string { return strings.Repeat("é", n) }
	nul := func(n int) string { return "x\x00" + strings.Repeat("y", n-2) }
	const (
		task = "INSERT INTO tasks VALUES (?, 0, 'q', 0, ?, 0, NULL, 0, 0, 0, '')"
		doc  = "INSERT INTO docs VALUES (?, ?, 0, ?, 0, ?, ?, NULL, 0, 0)"
	)
	// Every row has its own primary key, so only a CHECK can reject it.
	tests := []struct {
		name  string
		query string
		args  []any
	}{
		{"task id multibyte", task, []any{wide(33), ""}},
		{"task id NUL", task, []any{nul(65), ""}},
		{"task claimant multibyte", task, []any{"t1", wide(33)}},
		{"doc namespace NUL", doc, []any{nul(1025), "d1", "", "k", ""}},
		{"doc id multibyte", doc, []any{"ns", wide(33), "", "k", ""}},
		{"doc claimant NUL", doc, []any{"ns", "d2", nul(65), "k", ""}},
		{"doc key multibyte", doc, []any{"ns", "d3", "", wide(129), ""}},
		{"doc secondary key NUL", doc, []any{"ns", "d4", "", "k", nul(257)}},
	}
	for _, test := range tests {
		if _, err := db.ExecContext(ctx, test.query, test.args...); err == nil {
			t.Errorf("schema accepted over-limit value: %s", test.name)
		}
	}
	// Version 1 capped namespaces at 64 characters; version 2 allows 1024 bytes.
	if _, err := db.ExecContext(ctx, doc, strings.Repeat("n", 1024), "d5", "", "k", ""); err != nil {
		t.Errorf("schema rejected a 1024-byte namespace: %v", err)
	}
}

// TestSchemaChecksCountBytes covers the SQLite length() bug directly: it
// counts characters and stops at the first NUL, so the schema admitted
// multibyte values up to twice the limit and ignored anything after a NUL.
func TestSchemaChecksCountBytes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "entroq.sqlite")
	b, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	checkSchemaRejects(ctx, t, path)
}

func TestMigrateV1ToV2(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "entroq.sqlite")
	execRaw(ctx, t, path, schemaV1,
		`INSERT INTO tasks VALUES ('task-1', 3, 'q', 0, 'worker', 2, '"v"', 10, 20, 1, 'boom')`,
		`INSERT INTO docs VALUES ('ns', 'doc-1', 4, '', 0, 'k', 's', '"c"', 30, 40)`,
	)

	client, err := entroq.New(ctx, Opener(path))
	if err != nil {
		t.Fatalf("open version 1 database: %v", err)
	}
	if v := rawSchemaVersion(ctx, t, path); v != SchemaVersion {
		t.Fatalf("schema version after migration = %d, want %d", v, SchemaVersion)
	}

	tasks, err := client.Tasks(ctx, "q")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks after migration: %v, %v", tasks, err)
	}
	if got := tasks[0]; got.ID != "task-1" || got.Version != 3 || got.Claimant != "worker" || got.Claims != 2 || got.Attempt != 1 || got.Err != "boom" {
		t.Fatalf("migrated task = %#v", got)
	}
	docs, err := client.Docs(ctx, &entroq.DocQuery{Namespace: "ns"})
	if err != nil || len(docs) != 1 {
		t.Fatalf("docs after migration: %v, %v", docs, err)
	}
	// Migrating on to version 3 gives the doc's group a lock one version past
	// its highest member.
	if got := docs[0]; got.ID != "doc-1" || got.Version != 5 || got.Key != "k" || got.SecondaryKey != "s" {
		t.Fatalf("migrated doc = %#v", got)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	// The rebuilt tables carry the version 2 CHECKs.
	checkSchemaRejects(ctx, t, path)
}

func TestMigrateV1ToV2RollsBackOversizedRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "entroq.sqlite")
	// 64 characters satisfied the version 1 CHECK, but 128 bytes does not
	// satisfy version 2.
	wide := strings.Repeat("é", 64)
	execRaw(ctx, t, path, schemaV1,
		`INSERT INTO tasks VALUES ('`+wide+`', 1, 'q', 0, '', 0, NULL, 0, 0, 0, '')`,
	)

	if _, err := Open(ctx, path); err == nil {
		t.Fatal("migrated a database holding an over-limit id")
	} else if !strings.Contains(err.Error(), "copy tasks") {
		t.Fatalf("migration error does not name the failing step: %v", err)
	}
	if v := rawSchemaVersion(ctx, t, path); v != 1 {
		t.Fatalf("schema version after failed migration = %d, want 1", v)
	}
	execRaw(ctx, t, path, "SELECT id FROM tasks WHERE id = '"+wide+"'")
}

// TestMigrateV2ToV3 checks that a version 2 database, where each doc carried
// its own version and claim, gains one lock per doc group, one version past
// the group's highest member, with any claim released.
func TestMigrateV2ToV3(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "entroq.sqlite")
	b, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour).UnixMilli()
	execRaw(ctx, t, path,
		"DROP TABLE doc_locks",
		"UPDATE entroq_meta SET schema_version = 2 WHERE id = 1",
		`INSERT INTO docs VALUES ('ns', 'a', 2, '', 0, 'k', '1', '"a"', 1, 1)`,
		fmt.Sprintf(`INSERT INTO docs VALUES ('ns', 'b', 7, 'holder', %d, 'k', '2', '"b"', 1, 1)`, future),
		`INSERT INTO docs VALUES ('ns', 'c', 0, '', 0, 'other', '', '"c"', 1, 1)`,
	)

	client, err := entroq.New(ctx, Opener(path))
	if err != nil {
		t.Fatalf("open version 2 database: %v", err)
	}
	defer client.Close()
	if v := rawSchemaVersion(ctx, t, path); v != SchemaVersion {
		t.Fatalf("schema version after migration = %d, want %d", v, SchemaVersion)
	}

	docs, err := client.Docs(ctx, &entroq.DocQuery{Namespace: "ns"})
	if err != nil || len(docs) != 3 {
		t.Fatalf("docs after migration: %v, %v", docs, err)
	}
	want := map[string]int32{"a": 8, "b": 8, "c": 1}
	for _, d := range docs {
		if d.Version != want[d.ID] || d.Claimant != "" {
			t.Errorf("migrated doc %q: want version %d and no claimant, got version %d, claimant %q", d.ID, want[d.ID], d.Version, d.Claimant)
		}
	}
	// The migrated group is writable at its lock's version.
	if _, err := client.Modify(ctx, docs[0].Change(entroq.WithContent("after"))); err != nil {
		t.Errorf("change after migration: %v", err)
	}
}
