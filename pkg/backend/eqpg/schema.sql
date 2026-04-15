-- EntroQ PostgreSQL schema.
-- All statements are idempotent and can be re-run safely against an existing database.
-- Compatible with PostgreSQL 12+.
--
-- Public API (intended for direct use):
--   entroq.modify        -- atomically insert/change/delete/depend on tasks (JSONB)
--   entroq.try_claim     -- claim a task from one of several queues (pure SQL)
--   entroq.queues        -- queue statistics
--   entroq.tasks         -- task listing
--   entroq.docs       -- resource/state storage
--   entroq.channel_name  -- convert a queue name to a LISTEN/NOTIFY channel identifier
--
-- Internal functions (prefixed with _; used by Go backend or by public functions):
--   entroq._modify_arrays   -- parallel-array form of modify, called by Go backend
--   entroq._modify_docs  -- update storage resources
--   entroq._try_claim_one   -- claim from a single queue with bucket randomization
--   entroq._try_claim_bucket -- claim from a specific hash bucket range
--   entroq._claim_docs   -- claim specific storage resources
--   entroq.notify_ready_queues -- trigger notifications for tasks that reached their 'at' time

CREATE SCHEMA IF NOT EXISTS entroq;

-- Core table. id and claimant are TEXT with CHECK constraints limiting them
-- to 64 characters -- long enough for UUIDs, ULIDs, etc.
-- ID representation: task IDs and claimant IDs are stored as TEXT with a
-- CHECK constraint limiting them to 64 characters. 64 accommodates UUIDs (36),
-- ULIDs (26), and similar schemes while keeping indexes efficient.
-- Bucketing uses hashtext(id) & 255 -- a stable 8-bit hash value in [0,255] --
-- with an index on (queue, at, (hashtext(id) & 255)) for efficient range scans.
-- hashtext() is IMMUTABLE and available in all supported PostgreSQL versions.
CREATE TABLE IF NOT EXISTS entroq.tasks (
    id       TEXT                     PRIMARY KEY NOT NULL CHECK (length(id) <= 64),
    version  INTEGER                  NOT NULL DEFAULT 0,
    queue    TEXT                     NOT NULL DEFAULT '',
    at       TIMESTAMP WITH TIME ZONE NOT NULL,
    created  TIMESTAMP WITH TIME ZONE,
    modified TIMESTAMP WITH TIME ZONE NOT NULL,
    claimant TEXT                     CHECK (claimant IS NULL OR length(claimant) <= 64),
    value    JSONB,
    claims   INTEGER                  NOT NULL DEFAULT 0,
    attempt  INTEGER                  NOT NULL DEFAULT 0,
    err      TEXT                     NOT NULL DEFAULT ''
);

-- Resource Storage. Keyed by namespace + id.
-- key_primary and key_secondary provide range-scan and sorting capabilities.
-- at is used for claiming (locking).
CREATE TABLE IF NOT EXISTS entroq.docs (
    namespace     TEXT                     NOT NULL CHECK (length(namespace) <= 64),
    id            TEXT                     NOT NULL CHECK (length(id) <= 64),
    version       INTEGER                  NOT NULL DEFAULT 0,
    claimant      TEXT                     CHECK (claimant IS NULL OR length(claimant) <= 64),
    at            TIMESTAMP WITH TIME ZONE,
    key_primary   TEXT                     NOT NULL DEFAULT '' CHECK (length(key_primary) <= 256),
    key_secondary TEXT                     NOT NULL DEFAULT '' CHECK (length(key_secondary) <= 256),
    value         JSONB,
    created       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    modified      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, id)
);

-- Indexes.
CREATE INDEX IF NOT EXISTS byID        ON entroq.tasks (id);
CREATE INDEX IF NOT EXISTS byVersion   ON entroq.tasks (version);
CREATE INDEX IF NOT EXISTS byQueue     ON entroq.tasks (queue);
CREATE INDEX IF NOT EXISTS byQueueAt   ON entroq.tasks (queue, at);

-- Storage Indexes.
CREATE INDEX IF NOT EXISTS idx_docs_keys  ON entroq.docs (namespace, key_primary, key_secondary);

-- Bucket index: supports range-based bucket selection in _try_claim_bucket.
-- hashtext(id) & 255 gives a stable value in [0, 255] for any text ID.
-- The BETWEEN predicate in the claim function uses this index for range scans,
-- avoiding a full-queue scan while preserving dynamic bucket count selection.
CREATE INDEX IF NOT EXISTS byQueueAtBucket ON entroq.tasks (queue, at, (hashtext(id) & 255));

-- Readiness index: supports global range scans for future-bound tasks becoming ready.
CREATE INDEX IF NOT EXISTS byAt ON entroq.tasks (at, queue);

-- Peak claims index: supports O(1) peak detection in QueueStats.
CREATE INDEX IF NOT EXISTS byQueueClaims ON entroq.tasks (queue, claims DESC);

-- Composite types used by stored procedures.
-- PostgreSQL has no CREATE TYPE IF NOT EXISTS, so we use DO/EXCEPTION blocks.
DO $$ BEGIN
    CREATE TYPE entroq.task_id AS (
        id      text,
        version integer
    );
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE TYPE entroq.doc_id AS (
        namespace text,
        id        text,
        version   integer
    );
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- task_arg is used for both inserts and changes. For inserts, the
-- version field is ignored. Empty id means auto-generate; NULL at means use
-- the current transaction time.
DO $$ BEGIN
    CREATE TYPE entroq.task_arg AS (
        id      text,
        version integer,
        queue   text,
        at      timestamptz,
        value   jsonb,
        attempt integer,
        err     text
    );
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE TYPE entroq.doc_arg AS (
        namespace     text,
        id            text,
        version       integer,
        at            timestamptz,
        key_primary   text,
        key_secondary text,
        value         jsonb
    );
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- entroq._try_claim_bucket claims one available task whose ID hashes into the
-- given bucket range. The bucket is selected by:
--
--   (hashtext(id) & 255) BETWEEN lo AND hi
--
-- where lo = p_bucket * (256 / p_num_buckets) and hi = lo + (256 / p_num_buckets) - 1.
-- When p_num_buckets=1 the range is [0, 255], matching every task (no-op filter).
-- The byQueueAtBucket index covers (queue, at, hashtext(id) & 255), so this
-- resolves as a range scan rather than a full queue scan.
CREATE OR REPLACE FUNCTION entroq._try_claim_bucket(
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
    value    jsonb,
    claims   integer,
    attempt  integer,
    err      text
) LANGUAGE plpgsql AS $$
DECLARE
    v_lo integer := p_bucket       * (256 / p_num_buckets);
    v_hi integer := (p_bucket + 1) * (256 / p_num_buckets) - 1;
BEGIN
    RETURN QUERY
        UPDATE entroq.tasks
        SET
            version  = entroq.tasks.version + 1,
            claims   = entroq.tasks.claims + 1,
            at       = p_now + p_duration,
            claimant = p_claimant,
            modified = p_now
        WHERE (entroq.tasks.id, entroq.tasks.version) IN (
            SELECT t2.id, t2.version
            FROM entroq.tasks t2
            WHERE
                t2.queue = p_queue AND
                t2.at <= p_now AND
                (hashtext(t2.id) & 255) BETWEEN v_lo AND v_hi
            FOR UPDATE SKIP LOCKED
            LIMIT 1
        )
        RETURNING
            entroq.tasks.id, entroq.tasks.version, entroq.tasks.queue, entroq.tasks.at,
            entroq.tasks.created, entroq.tasks.modified, entroq.tasks.claimant,
            entroq.tasks.value, entroq.tasks.claims, entroq.tasks.attempt, entroq.tasks.err;
END;
$$;

-- entroq._try_claim_one selects a bucket based on available task count and
-- calls _try_claim_bucket. Falls back to an unfiltered claim
-- (p_num_buckets=1, range [0,255]) if the chosen bucket is empty.
--
-- See also:
--   https://dba.stackexchange.com/questions/69471/postgres-update-limit-1
--   https://blog.2ndquadrant.com/what-is-select-skip-locked-for-in-postgresql-9-5/
CREATE OR REPLACE FUNCTION entroq._try_claim_one(
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
    value    jsonb,
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
        SELECT t.id FROM entroq.tasks t
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
        SELECT * FROM entroq._try_claim_bucket(
            p_queue, p_claimant, p_duration, v_now, v_num_buckets, v_bucket
        );

    -- FOUND is true if RETURN QUERY emitted at least one row.
    IF FOUND THEN
        RETURN;
    END IF;

    -- Fallback: bucket was empty; retry with full range (num_buckets=1).
    RETURN QUERY
        SELECT * FROM entroq._try_claim_bucket(
            p_queue, p_claimant, p_duration, v_now, 1, 0
        );
END;
$$;

-- entroq.try_claim is NOT used by the Go backend, which performs the queue
-- shuffle and per-queue claim loop in Go to preserve inter-transaction
-- interleaving (important for fairness across queues of different sizes).
-- It is provided here for pure-SQL consumers or future database-side claim
-- strategies that do not require that interleaving property.
CREATE OR REPLACE FUNCTION entroq.try_claim(
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
    value    jsonb,
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
        RETURN QUERY SELECT * FROM entroq._try_claim_one(v_queue, p_claimant, p_duration);
        IF FOUND THEN RETURN; END IF;
    END LOOP;
END;
$$;

-- entroq._modify_arrays atomically applies inserts, changes, deletes, and
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
-- For a more ergonomic SQL interface, use entroq.modify (JSONB).
CREATE OR REPLACE FUNCTION entroq._modify_arrays(
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
    p_ins_values   text[],
    p_ins_attempts integer[],
    p_ins_errs     text[],
    -- changes: must exist at the given version, then updated
    p_chg_ids      text[],
    p_chg_vers     integer[],
    p_chg_queues   text[],
    p_chg_ats      timestamptz[],
    p_chg_values   text[],
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
    value    jsonb,
    claims   integer,
    attempt  integer,
    err      text
) LANGUAGE plpgsql AS $$
DECLARE
    v_now          timestamptz := now();
    v_missing      jsonb;
    v_mismatched   jsonb;
    v_collisions   jsonb;
    v_qset         text[];
    v_queue        text;
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
        SELECT t.id AS lck_id, t.version AS lck_ver FROM entroq.tasks t
        WHERE t.id = ANY(ARRAY(SELECT dep_id FROM all_deps))
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
    JOIN entroq.tasks t ON t.id = i.chk_id
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
    DELETE FROM entroq.tasks
    USING unnest(coalesce(p_del_ids, '{}'::text[]), coalesce(p_del_vers, '{}'::integer[])) AS d(del_id, del_ver)
    WHERE entroq.tasks.id = d.del_id AND entroq.tasks.version = d.del_ver;

    -- Inserts: empty string = auto-generate; Go zero time = use v_now.
    -- CTE wraps the INSERT so RETURNING * is unambiguous; the outer SELECT
    -- uses alias r.col to avoid RETURNS TABLE OUT-parameter shadowing.
    RETURN QUERY
        WITH r AS (
            INSERT INTO entroq.tasks (id, version, queue, at, claimant, value, created, modified, attempt, err)
            SELECT
                CASE WHEN ins_id = '' THEN encode(gen_random_bytes(8), 'hex') ELSE ins_id END,
                0,
                ins_queue,
                CASE WHEN ins_at = '0001-01-01 00:00:00+00'::timestamptz THEN v_now ELSE ins_at END,
                p_claimant,
                ins_value::jsonb,
                v_now, v_now,
                ins_attempt, ins_err
            FROM unnest(
                coalesce(p_ins_ids,      '{}'::text[]),
                coalesce(p_ins_queues,   '{}'::text[]),
                coalesce(p_ins_ats,      '{}'::timestamptz[]),
                coalesce(p_ins_values,   '{}'::text[]),
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
            UPDATE entroq.tasks
            SET
                version  = entroq.tasks.version + 1,
                modified = v_now,
                queue    = c.chg_queue,
                at       = c.chg_at,
                value    = c.chg_value::jsonb,
                attempt  = c.chg_attempt,
                err      = c.chg_err
            FROM unnest(
                coalesce(p_chg_ids,      '{}'::text[]),
                coalesce(p_chg_vers,     '{}'::integer[]),
                coalesce(p_chg_queues,   '{}'::text[]),
                coalesce(p_chg_ats,      '{}'::timestamptz[]),
                coalesce(p_chg_values,   '{}'::text[]),
                coalesce(p_chg_attempts, '{}'::integer[]),
                coalesce(p_chg_errs,     '{}'::text[])
            ) AS c(chg_id, chg_version, chg_queue, chg_at, chg_value, chg_attempt, chg_err)
            WHERE entroq.tasks.id = c.chg_id AND entroq.tasks.version = c.chg_version
            RETURNING *
        )
        SELECT 'changed'::text, r.id, r.version, r.queue, r.at,
            r.created, r.modified, r.claimant, r.value,
            r.claims, r.attempt, r.err
        FROM r;

    -- Batch Notifications.
    -- Fires once per unique queue in the transaction, instead of once per row.
    -- This deduplicates notifications and eliminates row-trigger overhead.
    FOR v_queue IN
        SELECT DISTINCT q FROM (
            SELECT unnest(coalesce(p_ins_queues, '{}'::text[])) as q, 
                   unnest(coalesce(p_ins_ats,    '{}'::timestamptz[])) as a
            UNION ALL
            SELECT unnest(coalesce(p_chg_queues, '{}'::text[])) as q,
                   unnest(coalesce(p_chg_ats,    '{}'::timestamptz[])) as a
        ) sub
        WHERE sub.a <= v_now
    LOOP
        PERFORM pg_notify(entroq.channel_name(v_queue), '');
    END LOOP;
END;
$$;

-- entroq.modify is the ergonomic public SQL API: accepts JSONB arrays of task
-- objects and delegates to _modify_arrays.
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
-- Returns the same tagged rows as _modify_arrays:
--   kind='inserted' or kind='changed', followed by all task fields.
CREATE OR REPLACE FUNCTION entroq.modify(
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
    value    jsonb,
    claims   integer,
    attempt  integer,
    err      text
) LANGUAGE sql AS $$
    SELECT * FROM entroq._modify_arrays(
        p_claimant,
        -- depends
        ARRAY(SELECT e->>'id'                 FROM jsonb_array_elements(p_depends) e),
        ARRAY(SELECT (e->>'version')::integer  FROM jsonb_array_elements(p_depends) e),
        -- deletes
        ARRAY(SELECT e->>'id'                 FROM jsonb_array_elements(p_deletes) e),
        ARRAY(SELECT (e->>'version')::integer  FROM jsonb_array_elements(p_deletes) e),
        -- inserts: empty string triggers auto-generate in _modify_arrays
        ARRAY(SELECT coalesce(e->>'id', '')   FROM jsonb_array_elements(p_inserts) e),
        ARRAY(SELECT e->>'queue'              FROM jsonb_array_elements(p_inserts) e),
        ARRAY(SELECT coalesce((e->>'at')::timestamptz, '0001-01-01 00:00:00+00')
              FROM jsonb_array_elements(p_inserts) e),
        ARRAY(SELECT CASE WHEN e ? 'value' THEN (e->'value')::text ELSE NULL END
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
        ARRAY(SELECT CASE WHEN e ? 'value' THEN (e->'value')::text ELSE NULL END
              FROM jsonb_array_elements(p_changes) e),
        ARRAY(SELECT coalesce((e->>'attempt')::integer, 0)
              FROM jsonb_array_elements(p_changes) e),
        ARRAY(SELECT coalesce(e->>'err', '')
              FROM jsonb_array_elements(p_changes) e)
    )
$$;

-- entroq._modify_docs handles all doc table updates for an atomic modify call.
-- Used internally by entroq.modify and the Go backend.
-- Raises EQ001 on dependency failure.
CREATE OR REPLACE FUNCTION entroq._modify_docs(
    p_claimant     text,
    p_dep_ns       text[],
    p_dep_ids      text[],
    p_dep_vers     integer[],
    p_del_ns       text[],
    p_del_ids      text[],
    p_del_vers     integer[],
    p_ins_ns       text[],
    p_ins_ids      text[],
    p_ins_pkeys    text[],
    p_ins_skeys    text[],
    p_ins_values   text[],
    p_chg_ns       text[],
    p_chg_ids      text[],
    p_chg_vers     integer[],
    p_chg_pkeys    text[],
    p_chg_skeys    text[],
    p_chg_values   text[],
    p_chg_ats      timestamptz[]
) RETURNS TABLE(
    kind          text,
    namespace     text,
    id            text,
    version       integer,
    claimant      text,
    at            timestamptz,
    key_primary   text,
    key_secondary text,
    value         jsonb,
    created       timestamptz,
    modified      timestamptz
) LANGUAGE plpgsql AS $$
DECLARE
    v_now          timestamptz := now();
    v_missing      jsonb;
    v_mismatched   jsonb;
    v_collisions   jsonb;
BEGIN
    -- Dependency Checks
    WITH all_deps(dep_ns, dep_id, dep_ver) AS (
        SELECT * FROM unnest(coalesce(p_dep_ns, '{}'::text[]), coalesce(p_dep_ids, '{}'::text[]), coalesce(p_dep_vers, '{}'::integer[]))
        UNION ALL
        SELECT * FROM unnest(coalesce(p_del_ns, '{}'::text[]), coalesce(p_del_ids, '{}'::text[]), coalesce(p_del_vers, '{}'::integer[]))
        UNION ALL
        SELECT * FROM unnest(coalesce(p_chg_ns, '{}'::text[]), coalesce(p_chg_ids, '{}'::text[]), coalesce(p_chg_vers, '{}'::integer[]))
    ),
    locked AS (
        SELECT s.namespace AS lck_ns, s.id AS lck_id, s.version AS lck_ver, s.claimant AS lck_claimant, s.at AS lck_at
        FROM entroq.docs s
        JOIN all_deps d ON s.namespace = d.dep_ns AND s.id = d.dep_id
        FOR UPDATE
    )
    SELECT
        coalesce(jsonb_agg(jsonb_build_object('ns', d.dep_ns, 'id', d.dep_id, 'version', d.dep_ver)) FILTER (WHERE l.lck_id IS NULL), '[]'::jsonb),
        coalesce(jsonb_agg(jsonb_build_object('ns', d.dep_ns, 'id', d.dep_id, 'version', d.dep_ver))
                 FILTER (WHERE l.lck_id IS NOT NULL AND (l.lck_ver != d.dep_ver OR (l.lck_claimant IS NOT NULL AND l.lck_claimant != p_claimant AND l.lck_at > v_now))),
                 '[]'::jsonb)
    INTO v_missing, v_mismatched
    FROM all_deps d
    LEFT JOIN locked l ON l.lck_ns = d.dep_ns AND l.lck_id = d.dep_id;

    -- Collision Checks
    SELECT coalesce(jsonb_agg(jsonb_build_object('ns', i.ins_ns, 'id', i.ins_id)), '[]'::jsonb)
    INTO v_collisions
    FROM unnest(coalesce(p_ins_ns, '{}'::text[]), coalesce(p_ins_ids, '{}'::text[])) AS i(ins_ns, ins_id)
    JOIN entroq.docs s ON s.namespace = i.ins_ns AND s.id = i.ins_id;

    IF v_missing != '[]'::jsonb OR v_mismatched != '[]'::jsonb OR v_collisions != '[]'::jsonb THEN
        RAISE EXCEPTION 'entroq storage dependency error'
            USING ERRCODE = 'EQ001',
                  DETAIL  = jsonb_build_object('missing', v_missing, 'mismatched', v_mismatched, 'collisions', v_collisions)::text;
    END IF;

    -- Deletes
    DELETE FROM entroq.docs
    USING unnest(coalesce(p_del_ns, '{}'::text[]), coalesce(p_del_ids, '{}'::text[])) AS d(del_ns, del_id)
    WHERE entroq.docs.namespace = d.del_ns AND entroq.docs.id = d.del_id;

    -- Inserts
    RETURN QUERY
    WITH r AS (
        INSERT INTO entroq.docs (namespace, id, version, key_primary, key_secondary, value, created, modified)
        SELECT ins_ns, ins_id, 0, ins_pk, ins_sk, ins_val::jsonb, v_now, v_now
        FROM unnest(p_ins_ns, p_ins_ids, p_ins_pkeys, p_ins_skeys, p_ins_values)
        AS i(ins_ns, ins_id, ins_pk, ins_sk, ins_val)
        RETURNING *
    )
    SELECT 'inserted', r.namespace, r.id, r.version, r.claimant, r.at, r.key_primary, r.key_secondary, r.value, r.created, r.modified FROM r;

    -- Changes
    RETURN QUERY
    WITH r AS (
        UPDATE entroq.docs
        SET
            version = entroq.docs.version + 1,
            key_primary = c.chg_pk,
            key_secondary = c.chg_sk,
            value = c.chg_val::jsonb,
            at = c.chg_at,
            claimant = CASE WHEN c.chg_at IS NULL THEN NULL ELSE p_claimant END,
            modified = v_now
        FROM unnest(p_chg_ns, p_chg_ids, p_chg_vers, p_chg_pkeys, p_chg_skeys, p_chg_values, p_chg_ats)
        AS c(chg_ns, chg_id, chg_ver, chg_pk, chg_sk, chg_val, chg_at)
        WHERE entroq.docs.namespace = c.chg_ns AND entroq.docs.id = c.chg_id
        RETURNING *
    )
    SELECT 'changed', r.namespace, r.id, r.version, r.claimant, r.at, r.key_primary, r.key_secondary, r.value, r.created, r.modified FROM r;
END;
$$;

-- entroq.modify_docs is the ergonomic public SQL API for doc operations.
-- Mirrors entroq.modify but delegates to _modify_docs instead of _modify_arrays.
-- Callers needing atomic task+doc operations should wrap both in a transaction.
-- (Open question: whether a combined entroq.modify_all is worth adding.)
--
-- Each JSONB array element is an object with fields matching the operation:
--
--   depends / deletes:  {"namespace": "<ns>", "id": "<id>", "version": <int>}
--   inserts:            {"namespace": "<ns>", "id": "<id>",
--                        "key_primary": "<str>", "key_secondary": "<str>",
--                        "content": <jsonb>}
--   changes:            {"namespace": "<ns>", "id": "<id>", "version": <int>,
--                        "key_primary": "<str>", "key_secondary": "<str>",
--                        "content": <jsonb>, "at": "<rfc3339>"}
--
-- All parameters default to '[]'. Returns tagged rows from _modify_docs:
--   kind='inserted' or kind='changed', followed by all doc fields.
CREATE OR REPLACE FUNCTION entroq.modify_docs(
    p_claimant text,
    p_depends  jsonb DEFAULT '[]',
    p_deletes  jsonb DEFAULT '[]',
    p_inserts  jsonb DEFAULT '[]',
    p_changes  jsonb DEFAULT '[]'
) RETURNS TABLE(
    kind          text,
    namespace     text,
    id            text,
    version       integer,
    claimant      text,
    at            timestamptz,
    key_primary   text,
    key_secondary text,
    value         jsonb,
    created       timestamptz,
    modified      timestamptz
) LANGUAGE sql AS $$
    SELECT * FROM entroq._modify_docs(
        p_claimant,
        -- depends
        ARRAY(SELECT e->>'namespace'             FROM jsonb_array_elements(p_depends) e),
        ARRAY(SELECT e->>'id'                    FROM jsonb_array_elements(p_depends) e),
        ARRAY(SELECT (e->>'version')::integer    FROM jsonb_array_elements(p_depends) e),
        -- deletes
        ARRAY(SELECT e->>'namespace'             FROM jsonb_array_elements(p_deletes) e),
        ARRAY(SELECT e->>'id'                    FROM jsonb_array_elements(p_deletes) e),
        ARRAY(SELECT (e->>'version')::integer    FROM jsonb_array_elements(p_deletes) e),
        -- inserts
        ARRAY(SELECT e->>'namespace'             FROM jsonb_array_elements(p_inserts) e),
        ARRAY(SELECT e->>'id'                    FROM jsonb_array_elements(p_inserts) e),
        ARRAY(SELECT coalesce(e->>'key_primary',   '') FROM jsonb_array_elements(p_inserts) e),
        ARRAY(SELECT coalesce(e->>'key_secondary', '') FROM jsonb_array_elements(p_inserts) e),
        ARRAY(SELECT CASE WHEN e ? 'content' THEN (e->'content')::text ELSE NULL END
              FROM jsonb_array_elements(p_inserts) e),
        -- changes
        ARRAY(SELECT e->>'namespace'             FROM jsonb_array_elements(p_changes) e),
        ARRAY(SELECT e->>'id'                    FROM jsonb_array_elements(p_changes) e),
        ARRAY(SELECT (e->>'version')::integer    FROM jsonb_array_elements(p_changes) e),
        ARRAY(SELECT coalesce(e->>'key_primary',   '') FROM jsonb_array_elements(p_changes) e),
        ARRAY(SELECT coalesce(e->>'key_secondary', '') FROM jsonb_array_elements(p_changes) e),
        ARRAY(SELECT CASE WHEN e ? 'content' THEN (e->'content')::text ELSE NULL END
              FROM jsonb_array_elements(p_changes) e),
        ARRAY(SELECT (e->>'at')::timestamptz         FROM jsonb_array_elements(p_changes) e)
    )
$$;

-- entroq.queues returns queue statistics. If p_exact is non-empty it takes
-- precedence over p_prefix. p_limit=0 means no limit.
CREATE OR REPLACE FUNCTION entroq.queues(
    p_prefix text    DEFAULT '',
    p_exact  text[]  DEFAULT '{}',
    p_limit  integer DEFAULT 0
) RETURNS TABLE(
    name      text,
    num_tasks bigint
) LANGUAGE sql AS $$
    SELECT queue AS name, COUNT(*) AS num_tasks
    FROM entroq.tasks
    WHERE CASE
        WHEN cardinality(p_exact) > 0 THEN queue = ANY(p_exact)
        WHEN p_prefix != ''           THEN queue LIKE p_prefix || '%'
        ELSE true
    END
    GROUP BY queue
    ORDER BY queue
    LIMIT NULLIF(p_limit, 0)
$$;

-- entroq.tasks returns tasks ordered by at. p_queue='' means all queues.
-- p_omit_values=true returns empty bytea for the value column.
-- p_limit=0 means no limit.
CREATE OR REPLACE FUNCTION entroq.tasks(
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
    value    jsonb,
    claims   integer,
    attempt  integer,
    err      text
) LANGUAGE sql AS $$
    SELECT
        id, version, queue, at, created, modified, claimant,
        CASE WHEN p_omit_values THEN NULL::jsonb ELSE value END AS value,
        claims, attempt, err
    FROM entroq.tasks
    WHERE p_queue = '' OR queue = p_queue
    ORDER BY at
    LIMIT NULLIF(p_limit, 0)
$$;

-- entroq.channel_name converts a queue name into a valid PostgreSQL
-- LISTEN/NOTIFY channel identifier within the 63-byte limit.
-- Uses the full sanitized name when it fits; otherwise sandwiches an
-- 8-hex-char MD5 of the original name between a prefix and suffix.
-- Prefix length 25, suffix length 26: 2+25+1+8+1+26 = 63 bytes exactly.
--
-- md5() is used instead of CRC32 because it is a built-in PostgreSQL
-- function requiring no extension.
--
-- This function is part of the public API: use it when setting up your own
-- LISTEN subscriptions on entroq queues.
CREATE OR REPLACE FUNCTION entroq.channel_name(p_queue text)
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

-- entroq.docs lists docs in a namespace, optionally filtered by key range.
-- p_namespace='' means all namespaces.
-- p_key_start/p_key_end: half-open range [start, end) on key_primary.
-- p_key_end='' means no upper bound.
-- p_omit_values=true returns NULL for the value column.
CREATE OR REPLACE FUNCTION entroq.docs(
    p_namespace   text    DEFAULT '',
    p_key_start   text    DEFAULT '',
    p_key_end     text    DEFAULT '',
    p_limit       integer DEFAULT 0,
    p_omit_values boolean DEFAULT false
) RETURNS TABLE(
    namespace     text,
    id            text,
    version       integer,
    claimant      text,
    at            timestamptz,
    key_primary   text,
    key_secondary text,
    value         jsonb,
    created       timestamptz,
    modified      timestamptz
) LANGUAGE sql AS $$
    SELECT
        namespace, id, version, claimant, at,
        key_primary, key_secondary,
        CASE WHEN p_omit_values THEN NULL::jsonb ELSE value END AS value,
        created, modified
    FROM entroq.docs
    WHERE (p_namespace = '' OR namespace = p_namespace)
      AND (p_key_start = '' OR key_primary >= p_key_start)
      AND (p_key_end   = '' OR key_primary <  p_key_end)
    ORDER BY namespace, key_primary, key_secondary
    LIMIT NULLIF(p_limit, 0)
$$;

-- entroq._claim_docs claims all docs sharing a primary key in a namespace.
-- Raises EQ001 if any doc with that key is already claimed by another claimant,
-- with JSON detail {"missing_docs":[], "claimed_docs":[...]}.
-- Returns 0 rows (not an error) if no docs with the key exist.
CREATE OR REPLACE FUNCTION entroq._claim_docs(
    p_namespace text,
    p_claimant  text,
    p_duration  interval,
    p_key       text
) RETURNS TABLE(
    namespace     text,
    id            text,
    version       integer,
    claimant      text,
    at            timestamptz,
    key_primary   text,
    key_secondary text,
    value         jsonb,
    created       timestamptz,
    modified      timestamptz
) LANGUAGE plpgsql AS $$
DECLARE
    v_now     timestamptz := now();
    v_claimed jsonb;
BEGIN
    -- Lock all rows with this primary key to serialize concurrent claims.
    -- Use alias d to avoid ambiguity with RETURNS TABLE output column names.
    PERFORM d.id FROM entroq.docs AS d
    WHERE d.namespace = p_namespace AND d.key_primary = p_key
    FOR UPDATE;

    -- Detect any already-claimed docs.
    SELECT coalesce(
        jsonb_agg(jsonb_build_object('namespace', d.namespace, 'id', d.id, 'version', d.version))
        FILTER (WHERE d.claimant IS NOT NULL AND d.at > v_now AND d.claimant <> p_claimant),
        '[]'::jsonb
    )
    INTO v_claimed
    FROM entroq.docs AS d
    WHERE d.namespace = p_namespace AND d.key_primary = p_key;

    IF v_claimed != '[]'::jsonb THEN
        RAISE EXCEPTION 'entroq doc claim dependency error'
            USING ERRCODE = 'EQ001',
                  DETAIL  = jsonb_build_object(
                      'missing_docs', '[]'::jsonb,
                      'claimed_docs', v_claimed
                  )::text;
    END IF;

    -- plpgsql only allows DML in WITH clauses, not as subquery expressions.
    RETURN QUERY
        WITH updated AS (
            UPDATE entroq.docs
            SET
                version  = entroq.docs.version + 1,
                at       = v_now + p_duration,
                claimant = p_claimant,
                modified = v_now
            WHERE entroq.docs.namespace = p_namespace
              AND entroq.docs.key_primary = p_key
            RETURNING *
        )
        SELECT * FROM updated
        ORDER BY key_primary, key_secondary;
END;
$$;

-- entroq.claim_docs is the public interface for claiming docs.
-- See _claim_docs for parameter semantics.
CREATE OR REPLACE FUNCTION entroq.claim_docs(
    p_namespace text,
    p_claimant  text,
    p_duration  interval,
    p_key       text
) RETURNS TABLE(
    namespace     text,
    id            text,
    version       integer,
    claimant      text,
    at            timestamptz,
    key_primary   text,
    key_secondary text,
    value         jsonb,
    created       timestamptz,
    modified      timestamptz
) LANGUAGE sql AS $$
    SELECT * FROM entroq._claim_docs(p_namespace, p_claimant, p_duration, p_key)
$$;

-- Global state for readiness notifications. last_at tracks the watermark of
-- tasks that have already been processed for "silent readiness."
CREATE TABLE IF NOT EXISTS entroq.notification_state (
    id      INTEGER     PRIMARY KEY CHECK (id = 1),
    last_at TIMESTAMPTZ NOT NULL
);

-- Initialize the watermark if it doesn't exist.
INSERT INTO entroq.notification_state (id, last_at)
VALUES (1, now())
ON CONFLICT (id) DO NOTHING;



-- entroq.notify_ready_queues atomizes the "Check and Notify" logic by maintaining
-- a high-watermark of processed 'at' times. This makes the notification system
-- immune to ticker jitter, overlapping runs, or service restarts.
--
-- If p_min_interval is specified, the function only proceeds if the
-- watermark has stagnated for at least that long. This prevents distributed
-- tickers from flooding the database with redundant updates and signals.
--
-- pg_cron tip: if your PostgreSQL instance has pg_cron installed (e.g., RDS,
-- Cloud SQL, or self-hosted with the extension), you can schedule this function
-- to cover direct clients (such as entroq_pg.py) that do not run their own
-- heartbeat loop. Example:
--
--   SELECT cron.schedule('entroq-notify', '* * * * *',
--       $$SELECT entroq.notify_ready_queues('5 seconds')$$);
--
-- The p_min_interval argument ('5 seconds' above) prevents redundant work when
-- multiple backend instances or cron firings overlap within a minute. If the
-- eqpg gRPC backend is also running, its heartbeat and the cron job will
-- interleave harmlessly -- the watermark and interval guard ensure only one
-- caller does real work per interval.
CREATE OR REPLACE FUNCTION entroq.notify_ready_queues(p_min_interval interval DEFAULT '0 seconds')
RETURNS SETOF text LANGUAGE plpgsql AS $$
DECLARE
    v_old_last_at timestamptz;
    v_new_last_at timestamptz := now();
    v_queue       text;
BEGIN
    -- Lock the row and read the current watermark atomically.
    -- FOR UPDATE ensures concurrent callers serialize here; the second caller
    -- will see the updated last_at and bail out via the interval check below.
    SELECT last_at INTO v_old_last_at FROM entroq.notification_state WHERE id = 1 FOR UPDATE;

    -- Rate limit: skip if the watermark moved recently enough.
    -- This prevents distributed tickers from flooding notifications.
    IF now() - v_old_last_at < p_min_interval THEN
        RETURN;
    END IF;

    UPDATE entroq.notification_state SET last_at = v_new_last_at WHERE id = 1;
    
    FOR v_queue IN
        SELECT DISTINCT queue 
        FROM entroq.tasks 
        WHERE at > v_old_last_at AND at <= v_new_last_at
    LOOP
        PERFORM pg_notify(entroq.channel_name(v_queue), '');
        RETURN NEXT v_queue;
    END LOOP;
END;
$$;

-- Schema version tracking. Updated on every run of this script so that
-- re-applying the schema after a minor-version upgrade stamps the new version.
-- The backend reads this on startup and refuses to operate if it does not match
-- the compiled-in SchemaVersion constant.
--
-- Versioning policy (1.x+):
--   - Schema version changes only when the schema itself changes.
--   - Minor version bumps (1.x -> 1.y) may change the schema, but only
--     additively: new tables, columns with defaults, indexes, or functions.
--     No column renames, type changes, or data movement.
--   - Patch releases never change the schema.
--   - Upgrading from any 1.x schema to any later 1.y schema is always safe:
--     re-run this script and the new additions appear without touching existing
--     data.
--   - Schemas predating 1.0 (0.x) cannot be migrated. Drain all tasks and
--     reinitialize: DROP SCHEMA entroq CASCADE, then run eqpg schema init.
CREATE TABLE IF NOT EXISTS entroq.meta (
    key   TEXT PRIMARY KEY NOT NULL,
    value TEXT NOT NULL
);

INSERT INTO entroq.meta (key, value) VALUES ('schema_version', '1.0.0')
    ON CONFLICT (key) DO UPDATE SET value = '1.0.0' WHERE entroq.meta.key = 'schema_version';
