-- EntroQ PostgreSQL schema.
-- All statements are idempotent and can be re-run safely against an existing database.
-- Compatible with PostgreSQL 12+.
--
-- Steps must run in order: column migrations precede CREATE TABLE so that
-- existing databases gain new columns before the table creation no-ops on them.
--
-- ID representation: task IDs and claimant IDs are stored as TEXT with a
-- CHECK constraint limiting them to 64 characters. 64 accommodates UUIDs (36),
-- ULIDs (26), and similar schemes while keeping indexes efficient.
-- Bucketing uses hashtext(id) & 255 -- a stable 8-bit hash value in [0,255] --
-- with an index on (queue, at, (hashtext(id) & 255)) for efficient range scans.
-- hashtext() is IMMUTABLE and available in all supported PostgreSQL versions.

-- pgcrypto provides gen_random_uuid() on PostgreSQL < 13.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Column additions for databases predating the claims/attempt/err fields.
ALTER TABLE IF EXISTS tasks ADD COLUMN IF NOT EXISTS claims  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE IF EXISTS tasks ADD COLUMN IF NOT EXISTS attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE IF EXISTS tasks ADD COLUMN IF NOT EXISTS err     TEXT    NOT NULL DEFAULT '';

-- Column type migration: uuid -> text for id and claimant.
-- Every UUID has a canonical text representation so this cast is safe.
-- No-ops if the columns are already text.
ALTER TABLE IF EXISTS tasks ALTER COLUMN id       TYPE text USING id::text;
ALTER TABLE IF EXISTS tasks ALTER COLUMN claimant TYPE text USING claimant::text;

-- ID length constraints. Idempotent: drop then re-add.
ALTER TABLE IF EXISTS tasks DROP CONSTRAINT IF EXISTS id_max_len;
ALTER TABLE IF EXISTS tasks ADD  CONSTRAINT         id_max_len       CHECK (length(id) <= 64);
ALTER TABLE IF EXISTS tasks DROP CONSTRAINT IF EXISTS claimant_max_len;
ALTER TABLE IF EXISTS tasks ADD  CONSTRAINT         claimant_max_len CHECK (claimant IS NULL OR length(claimant) <= 64);

-- Drop old UUID-typed composite types; recreated below with text id fields.
-- These types are not used in any function signatures, so no CASCADE is needed.
DROP TYPE IF EXISTS entroq_task_id;
DROP TYPE IF EXISTS entroq_task_arg;

-- Drop any stored procedures incompatible with current table schema. Recreating them is non-disruptive.
DROP FUNCTION IF EXISTS entroq_try_claim_bucket(text, uuid, interval, timestamptz, integer, integer);
DROP FUNCTION IF EXISTS entroq_try_claim_one(text, uuid, interval);
DROP FUNCTION IF EXISTS entroq_try_claim(text[], uuid, interval);
DROP FUNCTION IF EXISTS entroq_modify_arrays(uuid, uuid[], integer[], uuid[], integer[], uuid[], text[], timestamptz[], bytea[], integer[], text[], uuid[], integer[], text[], timestamptz[], bytea[], integer[], text[]);
DROP FUNCTION IF EXISTS entroq_modify(uuid, jsonb, jsonb, jsonb, jsonb);
DROP FUNCTION IF EXISTS entroq_tasks(text, integer, boolean);

-- Core table. id and claimant are TEXT with CHECK constraints limiting them
-- to 64 characters -- long enough for UUIDs, ULIDs, etc.
CREATE TABLE IF NOT EXISTS tasks (
    id       TEXT                     PRIMARY KEY NOT NULL CHECK (length(id) <= 64),
    version  INTEGER                  NOT NULL DEFAULT 0,
    queue    TEXT                     NOT NULL DEFAULT '',
    at       TIMESTAMP WITH TIME ZONE NOT NULL,
    created  TIMESTAMP WITH TIME ZONE,
    modified TIMESTAMP WITH TIME ZONE NOT NULL,
    claimant TEXT                     CHECK (claimant IS NULL OR length(claimant) <= 64),
    value    BYTEA,
    claims   INTEGER                  NOT NULL DEFAULT 0,
    attempt  INTEGER                  NOT NULL DEFAULT 0,
    err      TEXT                     NOT NULL DEFAULT ''
);

-- Indexes.
CREATE INDEX IF NOT EXISTS byID        ON tasks (id);
CREATE INDEX IF NOT EXISTS byVersion   ON tasks (version);
CREATE INDEX IF NOT EXISTS byQueue     ON tasks (queue);
CREATE INDEX IF NOT EXISTS byQueueAt   ON tasks (queue, at);

-- Bucket index: supports range-based bucket selection in entroq_try_claim_bucket.
-- hashtext(id) & 255 gives a stable value in [0, 255] for any text ID.
-- The BETWEEN predicate in the claim function uses this index for range scans,
-- avoiding a full-queue scan while preserving dynamic bucket count selection.
CREATE INDEX IF NOT EXISTS byQueueAtBucket ON tasks (queue, at, (hashtext(id) & 255));

-- Composite types used by stored procedures.
-- PostgreSQL has no CREATE TYPE IF NOT EXISTS, so we use DO/EXCEPTION blocks.
DO $$ BEGIN
    CREATE TYPE entroq_task_id AS (
        id      text,
        version integer
    );
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- entroq_task_arg is used for both inserts and changes. For inserts, the
-- version field is ignored. Empty id means auto-generate; NULL at means use
-- the current transaction time.
DO $$ BEGIN
    CREATE TYPE entroq_task_arg AS (
        id      text,
        version integer,
        queue   text,
        at      timestamptz,
        value   bytea,
        attempt integer,
        err     text
    );
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- entroq_try_claim_bucket claims one available task whose ID hashes into the
-- given bucket range. The bucket is selected by:
--
--   (hashtext(id) & 255) BETWEEN lo AND hi
--
-- where lo = p_bucket * (256 / p_num_buckets) and hi = lo + (256 / p_num_buckets) - 1.
-- When p_num_buckets=1 the range is [0, 255], matching every task (no-op filter).
-- The byQueueAtBucket index covers (queue, at, hashtext(id) & 255), so this
-- resolves as a range scan rather than a full queue scan.
CREATE OR REPLACE FUNCTION entroq_try_claim_bucket(
    p_queue       text,
    p_claimant    text,
    p_duration    interval,
    p_now         timestamptz,
    p_num_buckets integer,
    p_bucket      integer
) RETURNS TABLE(
    id       text,
    version  integer,
    queue    text,
    at       timestamptz,
    created  timestamptz,
    modified timestamptz,
    claimant text,
    value    bytea,
    claims   integer,
    attempt  integer,
    err      text
) LANGUAGE plpgsql AS $$
DECLARE
    v_lo integer := p_bucket       * (256 / p_num_buckets);
    v_hi integer := (p_bucket + 1) * (256 / p_num_buckets) - 1;
BEGIN
    RETURN QUERY
        UPDATE tasks
        SET
            version  = tasks.version + 1,
            claims   = tasks.claims + 1,
            at       = p_now + p_duration,
            claimant = p_claimant,
            modified = p_now
        WHERE (tasks.id, tasks.version) IN (
            SELECT t2.id, t2.version
            FROM tasks t2
            WHERE
                t2.queue = p_queue AND
                t2.at <= p_now AND
                (hashtext(t2.id) & 255) BETWEEN v_lo AND v_hi
            FOR UPDATE SKIP LOCKED
            LIMIT 1
        )
        RETURNING
            tasks.id, tasks.version, tasks.queue, tasks.at,
            tasks.created, tasks.modified, tasks.claimant,
            tasks.value, tasks.claims, tasks.attempt, tasks.err;
END;
$$;

-- entroq_try_claim_one selects a bucket based on available task count and
-- calls entroq_try_claim_bucket. Falls back to an unfiltered claim
-- (p_num_buckets=1, range [0,255]) if the chosen bucket is empty.
--
-- See also:
--   https://dba.stackexchange.com/questions/69471/postgres-update-limit-1
--   https://blog.2ndquadrant.com/what-is-select-skip-locked-for-in-postgresql-9-5/
CREATE OR REPLACE FUNCTION entroq_try_claim_one(
    p_queue     text,
    p_claimant  text,
    p_duration  interval
) RETURNS TABLE(
    id       text,
    version  integer,
    queue    text,
    at       timestamptz,
    created  timestamptz,
    modified timestamptz,
    claimant text,
    value    bytea,
    claims   integer,
    attempt  integer,
    err      text
) LANGUAGE plpgsql AS $$
DECLARE
    v_now         timestamptz := now();
    v_n_avail     integer;
    v_num_buckets integer;
    v_bucket      integer;
BEGIN
    -- Count available tasks up to 9 -- enough to distinguish bucket thresholds
    -- (1, 8) without scanning the full index for large queues.
    SELECT COUNT(*) INTO v_n_avail
    FROM (
        SELECT t.id FROM tasks t
        WHERE t.queue = p_queue AND t.at <= v_now
        LIMIT 9
    ) sub;

    IF v_n_avail = 0 THEN
        RETURN;
    END IF;

    -- Buckets are used as a cheap "ORDER BY random()" without resorting to a
    -- full table scan. hashtext(id) & 255 maps each task to [0, 255], and a
    -- random bucket range is selected before querying. The byQueueAtBucket
    -- index makes this a range scan. 1 bucket for a single task (full range,
    -- no-op filter), 2 for small queues, 4 for larger.
    CASE
        WHEN v_n_avail <= 1 THEN v_num_buckets := 1;
        WHEN v_n_avail <= 8 THEN v_num_buckets := 2;
        ELSE                     v_num_buckets := 4;
    END CASE;
    v_bucket := floor(random() * v_num_buckets)::integer;

    RETURN QUERY
        SELECT * FROM entroq_try_claim_bucket(
            p_queue, p_claimant, p_duration, v_now, v_num_buckets, v_bucket
        );

    -- FOUND is true if RETURN QUERY emitted at least one row.
    IF FOUND THEN
        RETURN;
    END IF;

    -- Fallback: bucket was empty; retry with full range (num_buckets=1).
    RETURN QUERY
        SELECT * FROM entroq_try_claim_bucket(
            p_queue, p_claimant, p_duration, v_now, 1, 0
        );
END;
$$;

-- entroq_try_claim is NOT used by the Go backend, which performs the queue
-- shuffle and per-queue claim loop in Go to preserve inter-transaction
-- interleaving (important for fairness across queues of different sizes).
-- It is provided here for pure-SQL consumers or future database-side claim
-- strategies that do not require that interleaving property.
--
CREATE OR REPLACE FUNCTION entroq_try_claim(
    p_queues    text[],
    p_claimant  text,
    p_duration  interval
) RETURNS TABLE(
    id       text,
    version  integer,
    queue    text,
    at       timestamptz,
    created  timestamptz,
    modified timestamptz,
    claimant text,
    value    bytea,
    claims   integer,
    attempt  integer,
    err      text
) LANGUAGE plpgsql AS $$
DECLARE
    v_queue  text;
    v_queues text[] := p_queues;
    v_n      integer := array_length(p_queues, 1);
    v_j      integer;
    v_tmp    text;
BEGIN
    -- Fisher-Yates shuffle. ORDER BY random() in a cursor loop can be
    -- treated as a constant by the query planner, producing no shuffle.
    FOR v_i IN REVERSE v_n..2 LOOP
        v_j := 1 + floor(random() * v_i)::integer;
        v_tmp         := v_queues[v_i];
        v_queues[v_i] := v_queues[v_j];
        v_queues[v_j] := v_tmp;
    END LOOP;

    FOREACH v_queue IN ARRAY v_queues LOOP
        RETURN QUERY SELECT * FROM entroq_try_claim_one(v_queue, p_claimant, p_duration);
        IF FOUND THEN RETURN; END IF;
    END LOOP;
END;
$$;

-- entroq_modify_arrays atomically applies inserts, changes, deletes, and
-- dependency checks. Uses parallel arrays for efficiency from the Go caller,
-- which avoids composite literal encoding complexity (especially bytea).
--
-- Raises SQLSTATE EQ001 with a JSON detail on any dependency problem.
-- The detail has three arrays:
--   'missing':    must-exist deps not found at all
--   'mismatched': must-exist deps found at wrong version
--   'collisions': explicit insert IDs that already exist
-- All three are checked before raising, so the caller sees all problems at once.
--
-- Returns tagged rows: kind='inserted' or kind='changed'.
-- Deleted tasks produce no output rows.
--
-- Insert sentinel: empty string in p_ins_ids means auto-generate.
-- Timestamp sentinel: Go's zero time ('0001-01-01 00:00:00+00') means use now().
--
-- For a more ergonomic SQL interface, use entroq_modify (JSONB).
CREATE OR REPLACE FUNCTION entroq_modify_arrays(
    p_claimant     text,
    -- depends: must exist at the given version
    p_dep_ids      text[],
    p_dep_vers     integer[],
    -- deletes: must exist at the given version, then removed
    p_del_ids      text[],
    p_del_vers     integer[],
    -- inserts: empty string = auto-generate, zero timestamptz = now()
    p_ins_ids      text[],
    p_ins_queues   text[],
    p_ins_ats      timestamptz[],
    p_ins_values   bytea[],
    p_ins_attempts integer[],
    p_ins_errs     text[],
    -- changes: must exist at the given version, then updated
    p_chg_ids      text[],
    p_chg_vers     integer[],
    p_chg_queues   text[],
    p_chg_ats      timestamptz[],
    p_chg_values   bytea[],
    p_chg_attempts integer[],
    p_chg_errs     text[]
) RETURNS TABLE(
    kind     text,
    id       text,
    version  integer,
    queue    text,
    at       timestamptz,
    created  timestamptz,
    modified timestamptz,
    claimant text,
    value    bytea,
    claims   integer,
    attempt  integer,
    err      text
) LANGUAGE plpgsql AS $$
DECLARE
    v_now          timestamptz := now();
    v_missing      jsonb;
    v_mismatched   jsonb;
    v_collisions   jsonb;
BEGIN
    -- Lock all must-exist dependency rows and check their versions.
    -- all_deps covers depends, deletes, and changes.
    -- locked acquires FOR UPDATE on matching rows.
    -- The LEFT JOIN finds missing rows (l.lck_id IS NULL) and version
    -- mismatches (l.lck_ver != d.dep_ver).
    --
    -- All CTE column aliases use prefixed names (dep_*, lck_*, etc.) to avoid
    -- ambiguity with the RETURNS TABLE OUT parameters (id, version, queue, ...)
    -- that PL/pgSQL puts in scope for the entire function body.
    WITH all_deps(dep_id, dep_ver) AS (
        SELECT * FROM unnest(coalesce(p_dep_ids, '{}'::text[]), coalesce(p_dep_vers, '{}'::integer[]))
        UNION ALL
        SELECT * FROM unnest(coalesce(p_del_ids, '{}'::text[]), coalesce(p_del_vers, '{}'::integer[]))
        UNION ALL
        SELECT * FROM unnest(coalesce(p_chg_ids, '{}'::text[]), coalesce(p_chg_vers, '{}'::integer[]))
    ),
    locked AS (
        SELECT tasks.id AS lck_id, tasks.version AS lck_ver FROM tasks
        WHERE tasks.id = ANY(ARRAY(SELECT dep_id FROM all_deps))
        FOR UPDATE
    )
    SELECT
        coalesce(
            jsonb_agg(jsonb_build_object('id', d.dep_id, 'version', d.dep_ver))
                FILTER (WHERE l.lck_id IS NULL),
            '[]'::jsonb
        ),
        coalesce(
            jsonb_agg(jsonb_build_object('id', d.dep_id, 'version', d.dep_ver))
                FILTER (WHERE l.lck_id IS NOT NULL AND l.lck_ver != d.dep_ver),
            '[]'::jsonb
        )
    INTO v_missing, v_mismatched
    FROM all_deps d
    LEFT JOIN locked l ON l.lck_id = d.dep_id;

    -- Check explicit insert ID conflicts: these must not already exist.
    -- No locking needed; the INSERT's PRIMARY KEY constraint handles races.
    SELECT coalesce(
        jsonb_agg(jsonb_build_object('id', i.chk_id, 'version', t.version)),
        '[]'::jsonb
    )
    INTO v_collisions
    FROM unnest(coalesce(p_ins_ids, '{}'::text[])) AS i(chk_id)
    JOIN tasks t ON t.id = i.chk_id
    WHERE i.chk_id != '';

    -- Report all problems at once.
    IF v_missing != '[]'::jsonb OR v_mismatched != '[]'::jsonb OR v_collisions != '[]'::jsonb THEN
        RAISE EXCEPTION 'entroq dependency error'
            USING ERRCODE = 'EQ001',
                  DETAIL  = jsonb_build_object(
                      'missing',    v_missing,
                      'mismatched', v_mismatched,
                      'collisions', v_collisions
                  )::text;
    END IF;

    -- Deletes: versions already verified; delete by id+version for safety.
    DELETE FROM tasks
    USING unnest(coalesce(p_del_ids, '{}'::text[]), coalesce(p_del_vers, '{}'::integer[])) AS d(del_id, del_ver)
    WHERE tasks.id = d.del_id AND tasks.version = d.del_ver;

    -- Inserts: empty string = auto-generate; Go zero time = use v_now.
    -- CTE wraps the INSERT so RETURNING * is unambiguous; the outer SELECT
    -- uses alias r.col to avoid RETURNS TABLE OUT-parameter shadowing.
    RETURN QUERY
        WITH r AS (
            INSERT INTO tasks (id, version, queue, at, claimant, value, created, modified, attempt, err)
            SELECT
                CASE WHEN ins_id = '' THEN gen_random_uuid()::text ELSE ins_id END,
                0,
                ins_queue,
                CASE WHEN ins_at = '0001-01-01 00:00:00+00'::timestamptz THEN v_now ELSE ins_at END,
                p_claimant,
                ins_value,
                v_now, v_now,
                ins_attempt, ins_err
            FROM unnest(
                coalesce(p_ins_ids,      '{}'::text[]),
                coalesce(p_ins_queues,   '{}'::text[]),
                coalesce(p_ins_ats,      '{}'::timestamptz[]),
                coalesce(p_ins_values,   '{}'::bytea[]),
                coalesce(p_ins_attempts, '{}'::integer[]),
                coalesce(p_ins_errs,     '{}'::text[])
            ) AS ins(ins_id, ins_queue, ins_at, ins_value, ins_attempt, ins_err)
            RETURNING *
        )
        SELECT 'inserted'::text, r.id, r.version, r.queue, r.at,
            r.created, r.modified, r.claimant, r.value,
            r.claims, r.attempt, r.err
        FROM r;

    -- Changes: update each task to its new values, incrementing version.
    -- CTE for the same reason as inserts: avoid RETURNS TABLE OUT-parameter shadowing.
    RETURN QUERY
        WITH r AS (
            UPDATE tasks
            SET
                version  = tasks.version + 1,
                modified = v_now,
                queue    = c.chg_queue,
                at       = c.chg_at,
                value    = c.chg_value,
                attempt  = c.chg_attempt,
                err      = c.chg_err
            FROM unnest(
                coalesce(p_chg_ids,      '{}'::text[]),
                coalesce(p_chg_vers,     '{}'::integer[]),
                coalesce(p_chg_queues,   '{}'::text[]),
                coalesce(p_chg_ats,      '{}'::timestamptz[]),
                coalesce(p_chg_values,   '{}'::bytea[]),
                coalesce(p_chg_attempts, '{}'::integer[]),
                coalesce(p_chg_errs,     '{}'::text[])
            ) AS c(chg_id, chg_version, chg_queue, chg_at, chg_value, chg_attempt, chg_err)
            WHERE tasks.id = c.chg_id AND tasks.version = c.chg_version
            RETURNING *
        )
        SELECT 'changed'::text, r.id, r.version, r.queue, r.at,
            r.created, r.modified, r.claimant, r.value,
            r.claims, r.attempt, r.err
        FROM r;
END;
$$;

-- entroq_modify is the ergonomic public SQL API: accepts JSONB arrays of task
-- objects and delegates to entroq_modify_arrays.
--
-- Each JSONB array element is an object with fields matching the operation:
--
--   depends / deletes:  {"id": "<id>", "version": <int>}
--   inserts:            {"queue": "<name>", "at": "<rfc3339>",
--                        "value": "<base64>", "attempt": <int>, "err": "<str>"}
--                       id and at are optional (omit/null -> auto-generate / now()).
--   changes:            {"id": "<id>", "version": <int>, "queue": "<name>",
--                        "at": "<rfc3339>", "value": "<base64>",
--                        "attempt": <int>, "err": "<str>"}
--
-- All parameters default to '[]' so callers only need to supply the operations
-- they actually want.
--
-- Returns the same tagged rows as entroq_modify_arrays:
--   kind='inserted' or kind='changed', followed by all task fields.
CREATE OR REPLACE FUNCTION entroq_modify(
    p_claimant text,
    p_depends  jsonb DEFAULT '[]',
    p_deletes  jsonb DEFAULT '[]',
    p_inserts  jsonb DEFAULT '[]',
    p_changes  jsonb DEFAULT '[]'
) RETURNS TABLE(
    kind     text,
    id       text,
    version  integer,
    queue    text,
    at       timestamptz,
    created  timestamptz,
    modified timestamptz,
    claimant text,
    value    bytea,
    claims   integer,
    attempt  integer,
    err      text
) LANGUAGE sql AS $$
    SELECT * FROM entroq_modify_arrays(
        p_claimant,
        -- depends
        ARRAY(SELECT e->>'id'                 FROM jsonb_array_elements(p_depends) e),
        ARRAY(SELECT (e->>'version')::integer  FROM jsonb_array_elements(p_depends) e),
        -- deletes
        ARRAY(SELECT e->>'id'                 FROM jsonb_array_elements(p_deletes) e),
        ARRAY(SELECT (e->>'version')::integer  FROM jsonb_array_elements(p_deletes) e),
        -- inserts: empty string triggers auto-generate in entroq_modify_arrays
        ARRAY(SELECT coalesce(e->>'id', '')   FROM jsonb_array_elements(p_inserts) e),
        ARRAY(SELECT e->>'queue'              FROM jsonb_array_elements(p_inserts) e),
        ARRAY(SELECT coalesce((e->>'at')::timestamptz, '0001-01-01 00:00:00+00')
              FROM jsonb_array_elements(p_inserts) e),
        ARRAY(SELECT decode(coalesce(e->>'value', ''), 'base64')
              FROM jsonb_array_elements(p_inserts) e),
        ARRAY(SELECT coalesce((e->>'attempt')::integer, 0)
              FROM jsonb_array_elements(p_inserts) e),
        ARRAY(SELECT coalesce(e->>'err', '')
              FROM jsonb_array_elements(p_inserts) e),
        -- changes
        ARRAY(SELECT e->>'id'                 FROM jsonb_array_elements(p_changes) e),
        ARRAY(SELECT (e->>'version')::integer  FROM jsonb_array_elements(p_changes) e),
        ARRAY(SELECT e->>'queue'              FROM jsonb_array_elements(p_changes) e),
        ARRAY(SELECT (e->>'at')::timestamptz  FROM jsonb_array_elements(p_changes) e),
        ARRAY(SELECT decode(coalesce(e->>'value', ''), 'base64')
              FROM jsonb_array_elements(p_changes) e),
        ARRAY(SELECT coalesce((e->>'attempt')::integer, 0)
              FROM jsonb_array_elements(p_changes) e),
        ARRAY(SELECT coalesce(e->>'err', '')
              FROM jsonb_array_elements(p_changes) e)
    )
$$;

-- entroq_queues returns queue statistics. If p_exact is non-empty it takes
-- precedence over p_prefix. p_limit=0 means no limit.
CREATE OR REPLACE FUNCTION entroq_queues(
    p_prefix text    DEFAULT '',
    p_exact  text[]  DEFAULT '{}',
    p_limit  integer DEFAULT 0
) RETURNS TABLE(
    name      text,
    num_tasks bigint
) LANGUAGE sql AS $$
    SELECT queue AS name, COUNT(*) AS num_tasks
    FROM tasks
    WHERE CASE
        WHEN cardinality(p_exact) > 0 THEN queue = ANY(p_exact)
        WHEN p_prefix != ''           THEN queue LIKE p_prefix || '%'
        ELSE true
    END
    GROUP BY queue
    ORDER BY queue
    LIMIT NULLIF(p_limit, 0)
$$;

-- entroq_tasks returns tasks ordered by at. p_queue='' means all queues.
-- p_omit_values=true returns empty bytea for the value column.
-- p_limit=0 means no limit.
CREATE OR REPLACE FUNCTION entroq_tasks(
    p_queue        text    DEFAULT '',
    p_limit        integer DEFAULT 0,
    p_omit_values  boolean DEFAULT false
) RETURNS TABLE(
    id       text,
    version  integer,
    queue    text,
    at       timestamptz,
    created  timestamptz,
    modified timestamptz,
    claimant text,
    value    bytea,
    claims   integer,
    attempt  integer,
    err      text
) LANGUAGE sql AS $$
    SELECT
        id, version, queue, at, created, modified, claimant,
        CASE WHEN p_omit_values THEN ''::bytea ELSE value END AS value,
        claims, attempt, err
    FROM tasks
    WHERE p_queue = '' OR queue = p_queue
    ORDER BY at
    LIMIT NULLIF(p_limit, 0)
$$;

-- entroq_channel_name converts a queue name into a valid PostgreSQL
-- LISTEN/NOTIFY channel identifier within the 63-byte limit.
-- Uses the full sanitized name when it fits; otherwise sandwiches an
-- 8-hex-char MD5 of the original name between a prefix and suffix.
-- Prefix length 25, suffix length 26: 2+25+1+8+1+26 = 63 bytes exactly.
--
-- md5() is used instead of CRC32 because it is a built-in PostgreSQL
-- function requiring no extension.
CREATE OR REPLACE FUNCTION entroq_channel_name(p_queue text)
RETURNS text LANGUAGE sql IMMUTABLE STRICT AS $$
    SELECT CASE
        WHEN length(regexp_replace(p_queue, '[^a-zA-Z0-9]', '_', 'g')) + 2 <= 63
        THEN 'q_' || regexp_replace(p_queue, '[^a-zA-Z0-9]', '_', 'g')
        ELSE 'q_'
            || left(regexp_replace(p_queue, '[^a-zA-Z0-9]', '_', 'g'), 25)
            || '_' || left(md5(p_queue), 8)
            || '_' || right(regexp_replace(p_queue, '[^a-zA-Z0-9]', '_', 'g'), 26)
    END
$$;

-- entroq_notify_task fires pg_notify on the queue's channel for a task that
-- is immediately available. The WHEN clause on the trigger filters to
-- NEW.at <= now(), so this function is only called when the task is already
-- claimable - no conditional needed inside the body.
CREATE OR REPLACE FUNCTION entroq_notify_task()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify(entroq_channel_name(NEW.queue), '');
    RETURN NULL;
END;
$$;

-- Trigger must be dropped before recreating because CREATE OR REPLACE TRIGGER
-- is only available in PostgreSQL 14+.
DROP TRIGGER IF EXISTS entroq_task_notify ON tasks;
CREATE TRIGGER entroq_task_notify
AFTER INSERT OR UPDATE OF at ON tasks
FOR EACH ROW
WHEN (NEW.at <= now())
EXECUTE FUNCTION entroq_notify_task();

-- Schema version tracking. Set once on first initialization; never overwritten
-- by subsequent re-runs of this script (ON CONFLICT DO NOTHING). The client
-- reads this on startup and refuses to operate if it does not match the
-- compiled-in SCHEMA_VERSION constant, protecting against accidental use of a
-- mismatched schema.
--
-- To migrate to a new version: apply the updated schema.sql, then manually
-- bump the stored version:
--   UPDATE entroq_meta SET value = '<new-version>' WHERE key = 'schema_version';
CREATE TABLE IF NOT EXISTS entroq_meta (
    key   TEXT PRIMARY KEY NOT NULL,
    value TEXT NOT NULL
);

INSERT INTO entroq_meta (key, value) VALUES ('schema_version', '0.10.0')
    ON CONFLICT (key) DO NOTHING;
