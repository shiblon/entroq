PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS entroq_meta (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    schema_version INTEGER NOT NULL
);

INSERT OR IGNORE INTO entroq_meta (id, schema_version) VALUES (1, 3);

-- Length limits count bytes (octet_length), matching the PostgreSQL schema.
-- SQLite's length() counts characters and stops at the first NUL, so it both
-- admits oversized multibyte values and ignores everything after a NUL.
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
    CHECK (octet_length(id) <= 64),
    CHECK (octet_length(claimant) <= 64)
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
    CHECK (octet_length(namespace) <= 1024),
    CHECK (octet_length(id) <= 64),
    CHECK (octet_length(claimant) <= 64),
    CHECK (octet_length(key_primary) <= 256),
    CHECK (octet_length(key_secondary) <= 256)
);

CREATE INDEX IF NOT EXISTS docs_namespace_keys
    ON docs (namespace, key_primary, key_secondary, id);

CREATE INDEX IF NOT EXISTS docs_namespace_at
    ON docs (namespace, at_ms, id);

-- Each doc group (the docs sharing a primary key in a namespace) has one lock
-- holding the only version, claimant, and arrival time its members have. A
-- group can be claimed before it has docs, so locks are kept apart from docs;
-- the docs table's version, claimant, and at_ms columns are no longer read.
CREATE TABLE IF NOT EXISTS doc_locks (
    namespace   TEXT NOT NULL COLLATE BINARY,
    key_primary TEXT NOT NULL COLLATE BINARY,
    version     INTEGER NOT NULL,
    claimant    TEXT NOT NULL COLLATE BINARY,
    at_ms       INTEGER NOT NULL,
    PRIMARY KEY (namespace, key_primary),
    CHECK (octet_length(namespace) <= 1024),
    CHECK (octet_length(key_primary) <= 256),
    CHECK (octet_length(claimant) <= 64)
);
