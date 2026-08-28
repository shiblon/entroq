PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS entroq_meta (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    schema_version INTEGER NOT NULL
);

INSERT OR IGNORE INTO entroq_meta (id, schema_version) VALUES (1, 1);

CREATE TABLE IF NOT EXISTS tasks (
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

CREATE INDEX IF NOT EXISTS tasks_queue_at
    ON tasks (queue, at_ms, id);

CREATE TABLE IF NOT EXISTS docs (
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

CREATE INDEX IF NOT EXISTS docs_namespace_keys
    ON docs (namespace, key_primary, key_secondary, id);

CREATE INDEX IF NOT EXISTS docs_namespace_at
    ON docs (namespace, at_ms, id);
