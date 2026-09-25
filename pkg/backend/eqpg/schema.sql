-- EntroQ PostgreSQL schema.
-- All statements are idempotent and can be re-run safely against an existing database.
-- Compatible with PostgreSQL 12+.
--
-- Backend functions used by the Go eqpg implementation:
--   entroq.try_claim       -- claim a task from one of several queues
--   entroq._modify_arrays   -- parallel-array form of modify, called by Go backend
--   entroq._modify_docs     -- atomically update storage resources
--   entroq._try_claim_one   -- claim from a single queue with bucket randomization
--   entroq._try_claim_bucket -- claim from a specific hash bucket range
--   entroq._claim_docs      -- atomically claim specific storage resources

CREATE SCHEMA IF NOT EXISTS entroq;

-- pgcrypto provides gen_random_bytes(), used for auto-generating task IDs.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Core table. Task IDs and claimant IDs are arbitrary TEXT with a CHECK
-- constraint limiting them to 64 bytes. They are NOT UUIDs: the default
-- generator emits short random strings. The 64-byte ceiling simply leaves room
-- for callers who elect to supply their own UUIDs (36 chars), ULIDs (26), or
-- similar schemes, while keeping indexes efficient.
-- Bucketing uses hashtext(id) & 255 -- a stable 8-bit hash value in [0,255] --
-- with an index on (queue, at, (hashtext(id) & 255)) for efficient range scans.
-- hashtext() is IMMUTABLE and available in all supported PostgreSQL versions.
-- The Go backend rejects writes to an empty queue or namespace through
-- entroq.Modification.EnsureModifyKeys. That API validation intentionally does
-- not need a second implementation in this private storage schema.
CREATE TABLE IF NOT EXISTS entroq.tasks (
    id       TEXT COLLATE "C"         PRIMARY KEY NOT NULL CHECK (octet_length(id) <= 64),
    version  INTEGER                  NOT NULL DEFAULT 0,
    queue    TEXT COLLATE "C"         NOT NULL DEFAULT '',
    at       TIMESTAMP WITH TIME ZONE NOT NULL,
    created  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    modified TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    claimant TEXT                     NOT NULL DEFAULT '' CHECK (octet_length(claimant) <= 64),
    value    JSONB,
    claims   INTEGER                  NOT NULL DEFAULT 0,
    attempt  INTEGER                  NOT NULL DEFAULT 0,
    err      TEXT                     NOT NULL DEFAULT ''
);

-- Resource Storage. Keyed by namespace + id.
-- key_primary and key_secondary provide range-scan and sorting capabilities.
-- at is used for claiming (locking).
CREATE TABLE IF NOT EXISTS entroq.docs (
    namespace     TEXT COLLATE "C"         NOT NULL CHECK (octet_length(namespace) <= 1024),
    id            TEXT COLLATE "C"         NOT NULL CHECK (octet_length(id) <= 64),
    version       INTEGER                  NOT NULL DEFAULT 0,
    claimant      TEXT                     NOT NULL DEFAULT '' CHECK (octet_length(claimant) <= 64),
    at            TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    key_primary   TEXT COLLATE "C"         NOT NULL DEFAULT '' CHECK (octet_length(key_primary) <= 256),
    key_secondary TEXT COLLATE "C"         NOT NULL DEFAULT '' CHECK (octet_length(key_secondary) <= 256),
    value         JSONB,
    created       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    modified      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, id)
);

-- Indexes.
CREATE INDEX IF NOT EXISTS byID        ON entroq.tasks (id);
CREATE INDEX IF NOT EXISTS byVersion   ON entroq.tasks (version);
CREATE INDEX IF NOT EXISTS byQueue     ON entroq.tasks (queue);
-- Claim range scans use the (queue, at) prefix; QueueStats additionally reads
-- claims in the same pass, so one covering index over (queue, at, claims) lets
-- it run as a single index-only grouped scan rather than a full heap scan. This
-- supersedes the separate (queue, at) and (queue, claims DESC) indexes; drop the
-- old names so a re-applied schema converges an existing database.
DROP INDEX IF EXISTS entroq.byQueueAt;
DROP INDEX IF EXISTS entroq.byQueueClaims;
CREATE INDEX IF NOT EXISTS byQueueAtClaims ON entroq.tasks (queue, at, claims);

-- GC is an ordinary Go backend claim/delete worker. Retire the old discovery
-- indexes; ordinary queue listing uses byQueue and exact claims use
-- byQueueAtClaims.
DROP INDEX IF EXISTS entroq.byGCQueueAt;
DROP INDEX IF EXISTS entroq.byCompoundGCQueueAt;

-- Storage Indexes.
CREATE INDEX IF NOT EXISTS idx_docs_keys     ON entroq.docs (namespace, key_primary, key_secondary);
-- Covers NamespaceStats: GROUP BY namespace with FILTER on at/claimant, index-only.
CREATE INDEX IF NOT EXISTS idx_docs_ns_stats ON entroq.docs (namespace, at, claimant);

-- Bucket index: supports range-based bucket selection in _try_claim_bucket.
-- hashtext(id) & 255 gives a stable value in [0, 255] for any text ID.
-- The BETWEEN predicate in the claim function uses this index for range scans,
-- avoiding a full-queue scan while preserving dynamic bucket count selection.
CREATE INDEX IF NOT EXISTS byQueueAtBucket ON entroq.tasks (queue, at, (hashtext(id) & 255));

-- The retired global readiness scan used byAt. The Go backend now counts ready
-- tasks per waited queue through byQueueAtClaims, and byAt only added an index
-- update to every task write.
DROP INDEX IF EXISTS entroq.byAt;

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

-- entroq.try_claim is the Go backend's single-statement multi-queue claim
-- coordinator. It randomizes queue order, then delegates each attempt to
-- _try_claim_one; the locking and atomic update live in _try_claim_bucket.
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
-- The queue is part of the modify key: depends and deletes must name the task's
-- current queue, and a change must name its source (from) queue, with
-- p_chg_queues carrying the destination. A mismatched or empty queue fails the
-- operation as a missing dependency (the queue authorizes access, so it must
-- not be silently filled in from stored state).
--
-- The signature gained the queue arrays in schema 1.7.1; drop the prior overload
-- first, since a changed argument list would otherwise leave the old function
-- behind on a re-applied schema.
DROP FUNCTION IF EXISTS entroq._modify_arrays(
    text,
    text[], integer[],
    text[], integer[],
    text[], text[], timestamptz[], text[], integer[], text[],
    text[], integer[], text[], timestamptz[], text[], integer[], text[]
);
CREATE OR REPLACE FUNCTION entroq._modify_arrays(
    p_claimant        text,
    -- depends: must exist at the given (version, queue)
    p_dep_ids         text[],
    p_dep_vers        integer[],
    p_dep_queues      text[],
    -- deletes: must exist at the given (version, queue), then removed
    p_del_ids         text[],
    p_del_vers        integer[],
    p_del_queues      text[],
    -- inserts: empty string = auto-generate, zero timestamptz = now()
    p_ins_ids         text[],
    p_ins_queues      text[],
    p_ins_ats         timestamptz[],
    p_ins_values      text[],
    p_ins_attempts    integer[],
    p_ins_errs        text[],
    -- changes: must exist at the given (version, from-queue), then updated.
    -- p_chg_from_queues is the source (matched); p_chg_queues is the destination.
    p_chg_ids         text[],
    p_chg_vers        integer[],
    p_chg_from_queues text[],
    p_chg_queues      text[],
    p_chg_ats         timestamptz[],
    p_chg_values      text[],
    p_chg_attempts    integer[],
    p_chg_errs        text[]
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
    -- all_deps carries the claimed queue per op: the task's current queue for
    -- depends/deletes, the source (from) queue for changes. The queue is part of
    -- the key, so the LEFT JOIN matches on (id, queue); a queue mismatch fails to
    -- join and surfaces as missing, indistinguishable from an absent task.
    WITH all_deps(dep_id, dep_ver, dep_queue) AS (
        SELECT * FROM unnest(coalesce(p_dep_ids, '{}'::text[]), coalesce(p_dep_vers, '{}'::integer[]), coalesce(p_dep_queues, '{}'::text[]))
        UNION ALL
        SELECT * FROM unnest(coalesce(p_del_ids, '{}'::text[]), coalesce(p_del_vers, '{}'::integer[]), coalesce(p_del_queues, '{}'::text[]))
        UNION ALL
        SELECT * FROM unnest(coalesce(p_chg_ids, '{}'::text[]), coalesce(p_chg_vers, '{}'::integer[]), coalesce(p_chg_from_queues, '{}'::text[]))
    ),
    locked AS (
        SELECT t.id AS lck_id, t.version AS lck_ver, t.queue AS lck_queue FROM entroq.tasks t
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
    LEFT JOIN locked l ON l.lck_id = d.dep_id AND l.lck_queue = d.dep_queue;

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
    USING unnest(coalesce(p_del_ids, '{}'::text[]), coalesce(p_del_vers, '{}'::integer[]), coalesce(p_del_queues, '{}'::text[])) AS d(del_id, del_ver, del_queue)
    WHERE entroq.tasks.id = d.del_id AND entroq.tasks.version = d.del_ver AND entroq.tasks.queue = d.del_queue;

    -- Inserts: empty string = auto-generate.
    -- at: timestamps older than 1 year are treated as "use now". This threshold
    -- avoids needing a sentinel value for Go's zero time (0001-01-01), which
    -- arrives as a far-past timestamp. Legitimate past timestamps (within a year)
    -- are preserved; a past at is harmless -- the task is immediately available.
    -- CTE wraps the INSERT so RETURNING * is unambiguous; the outer SELECT
    -- uses alias r.col to avoid RETURNS TABLE OUT-parameter shadowing.
    RETURN QUERY
        WITH r AS (
            INSERT INTO entroq.tasks (id, version, queue, at, claimant, value, created, modified, attempt, err)
            SELECT
                CASE WHEN ins_id = '' THEN encode(gen_random_bytes(8), 'hex') ELSE ins_id END,
                0,
                ins_queue,
                CASE WHEN ins_at < v_now - interval '1 year' THEN v_now ELSE ins_at END,
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

    -- Changes: at older than 1 year snaps to v_now (covers Go's zero time);
    -- otherwise preserved as-is (a past at is harmless). Claimant is set only
    -- when chg_at is strictly in the future; past/present releases the task.
    -- CTE for the same reason as inserts: avoid RETURNS TABLE OUT-parameter shadowing.
    RETURN QUERY
        WITH r AS (
            UPDATE entroq.tasks
            SET
                version  = entroq.tasks.version + 1,
                modified = v_now,
                queue    = c.chg_queue,
                at       = CASE WHEN c.chg_at < v_now - interval '1 year' THEN v_now ELSE c.chg_at END,
                value    = c.chg_value::jsonb,
                attempt  = c.chg_attempt,
                err      = c.chg_err,
                claimant = CASE WHEN c.chg_at > v_now THEN p_claimant ELSE '' END
            FROM unnest(
                coalesce(p_chg_ids,         '{}'::text[]),
                coalesce(p_chg_vers,        '{}'::integer[]),
                coalesce(p_chg_from_queues, '{}'::text[]),
                coalesce(p_chg_queues,      '{}'::text[]),
                coalesce(p_chg_ats,         '{}'::timestamptz[]),
                coalesce(p_chg_values,      '{}'::text[]),
                coalesce(p_chg_attempts,    '{}'::integer[]),
                coalesce(p_chg_errs,        '{}'::text[])
            ) AS c(chg_id, chg_version, chg_from_queue, chg_queue, chg_at, chg_value, chg_attempt, chg_err)
            WHERE entroq.tasks.id = c.chg_id AND entroq.tasks.version = c.chg_version AND entroq.tasks.queue = c.chg_from_queue
            RETURNING *
        )
        SELECT 'changed'::text, r.id, r.version, r.queue, r.at,
            r.created, r.modified, r.claimant, r.value,
            r.claims, r.attempt, r.err
        FROM r;
END;
$$;

-- Remove the retired raw-SQL task wrapper. The Go backend calls
-- _modify_arrays directly.
DROP FUNCTION IF EXISTS entroq.modify(text, jsonb, jsonb, jsonb, jsonb);

-- entroq._modify_docs handles all doc table updates for an atomic modify call.
-- Used by the Go backend.
-- Raises EQ001 on dependency failure.
-- Drop the public wrapper first so an upgrade can replace the internal
-- function's signature without leaving an obsolete overload behind.
DROP FUNCTION IF EXISTS entroq.modify_docs(text, jsonb, jsonb, jsonb, jsonb);
DROP FUNCTION IF EXISTS entroq._modify_docs(
    text,
    text[], text[], integer[],
    text[], text[], integer[],
    text[], text[], text[], text[], text[],
    text[], text[], integer[], text[], text[], text[], timestamptz[]
);
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
    p_ins_ats      timestamptz[],
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
                 FILTER (WHERE l.lck_id IS NOT NULL AND (l.lck_ver != d.dep_ver OR (l.lck_claimant != '' AND l.lck_claimant != p_claimant AND l.lck_at > v_now))),
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
        INSERT INTO entroq.docs (namespace, id, version, claimant, at, key_primary, key_secondary, value, created, modified)
        SELECT ins_ns, ins_id, 0,
               CASE WHEN ins_at > v_now THEN p_claimant ELSE '' END,
               CASE WHEN ins_at IS NULL OR ins_at < v_now - interval '1 year' THEN v_now ELSE ins_at END,
               ins_pk, ins_sk, ins_val::jsonb, v_now, v_now
        FROM unnest(p_ins_ns, p_ins_ids, p_ins_pkeys, p_ins_skeys, p_ins_values, p_ins_ats)
        AS i(ins_ns, ins_id, ins_pk, ins_sk, ins_val, ins_at)
        RETURNING *
    )
    SELECT 'inserted', r.namespace, r.id, r.version, r.claimant, r.at, r.key_primary, r.key_secondary, r.value, r.created, r.modified FROM r;

    -- Changes: future chg_at means claim/renew; past/zero means release.
    RETURN QUERY
    WITH r AS (
        UPDATE entroq.docs
        SET
            version = entroq.docs.version + 1,
            key_primary = c.chg_pk,
            key_secondary = c.chg_sk,
            value = c.chg_val::jsonb,
            -- at: >1 year old snaps to v_now (covers Go's zero time); otherwise preserved.
            at = CASE WHEN c.chg_at < v_now - interval '1 year' THEN v_now ELSE c.chg_at END,
            claimant = CASE WHEN c.chg_at > v_now THEN p_claimant ELSE '' END,
            modified = v_now
        FROM unnest(p_chg_ns, p_chg_ids, p_chg_vers, p_chg_pkeys, p_chg_skeys, p_chg_values, p_chg_ats)
        AS c(chg_ns, chg_id, chg_ver, chg_pk, chg_sk, chg_val, chg_at)
        WHERE entroq.docs.namespace = c.chg_ns AND entroq.docs.id = c.chg_id
        RETURNING *
    )
    SELECT 'changed', r.namespace, r.id, r.version, r.claimant, r.at, r.key_primary, r.key_secondary, r.value, r.created, r.modified FROM r;
END;
$$;

-- Remove read-only wrappers from the retired raw-SQL API. The Go backend
-- issues these parameterized queries directly.
DROP FUNCTION IF EXISTS entroq.queues(text, text[], integer);
DROP FUNCTION IF EXISTS entroq.tasks(text, integer, boolean);
DROP FUNCTION IF EXISTS entroq.like_prefix(text);

-- Remove the retired raw-SQL document listing wrapper. The Go backend
-- queries entroq.docs directly.
DROP FUNCTION IF EXISTS entroq.docs(text, text, text, integer, boolean);

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
        FILTER (WHERE d.claimant != '' AND d.at > v_now AND d.claimant <> p_claimant),
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

-- Remove the retired raw-SQL claim wrapper. The locking implementation
-- remains in _claim_docs for the Go backend.
DROP FUNCTION IF EXISTS entroq.claim_docs(text, text, interval, text);

-- Drop the composite argument types after every legacy wrapper that could
-- depend on them has been removed.
DROP TYPE IF EXISTS entroq.task_arg;
DROP TYPE IF EXISTS entroq.doc_arg;
DROP TYPE IF EXISTS entroq.task_id;
DROP TYPE IF EXISTS entroq.doc_id;

-- Retired LISTEN/NOTIFY readiness: the watermark table, the function that
-- scanned and broadcast from it, and the channel-name helper. Each Go backend
-- now runs its own readiness loop over the queues its claims wait on, and
-- modifications no longer send NOTIFY.
DROP FUNCTION IF EXISTS entroq.notify_ready_queues(interval);
DROP TABLE IF EXISTS entroq.notification_state;
DROP FUNCTION IF EXISTS entroq.channel_name(text);

-- GC is an ordinary Go backend queue worker. Remove the retired
-- PostgreSQL-specific policy and collection API so existing schemas converge on
-- the same claim/delete protocol used by every other backend.
DROP FUNCTION IF EXISTS entroq.gc_collect(text[], timestamptz[], integer);
DROP FUNCTION IF EXISTS entroq.gc_queues();
DROP FUNCTION IF EXISTS entroq.gc_activation(text);
DROP FUNCTION IF EXISTS entroq._path_param_values(text, text);
DROP FUNCTION IF EXISTS entroq.gc_due(text);

-- Schema version tracking. Updated on every run of this script so that
-- re-applying the schema after a minor-version upgrade stamps the new version.
-- The backend reads this on startup and refuses to operate if it does not match
-- the compiled-in SchemaVersion constant.
--
-- Versioning policy (1.x+):
--   - Schema version is the full version of the release where the schema last
--     changed. It may lag the module version, but must never exceed it.
--   - A schema change causes a minor bump. A minor release with no schema
--     change leaves the schema version alone.
--   - Additive changes (new tables, columns with defaults, indexes, or
--     functions) are always permitted. A non-additive migration is permitted
--     only when it is transparent to clients and this file applies it
--     idempotently; such migrations may require a maintenance window.
--   - Patch releases never change the schema.
--   - Upgrading from any 1.x schema to any later 1.y schema is supported by
--     re-running this file; see the release changelog for operational impact.
--   - Schemas predating 1.0 (0.x) cannot be migrated. Drain all tasks and
--     reinitialize: DROP SCHEMA entroq CASCADE, then run eqpg schema init.
-- Migrations: 1.0.0 → 1.1.0 (see blocks below)
-- Migrations: 1.1.0 → 1.2.0 (no structural changes)
-- Migrations: 1.2.0 → 1.6.0 (additive: byGCQueueAt partial index and the
--   gc_activation / gc_queues / gc_collect functions for built-in garbage
--   collection; drops the interim gc_due function; no data movement)
-- Migrations: 1.6.0 → 1.7.1 (two changes, shipped in one release; 1.7.0 is
--   skipped -- it named a queue-array-only schema that only ever existed on an
--   unreleased branch, so reusing it would let such a database skip the
--   collation step below):
--   (1) the queue joins the modify key: _modify_arrays gains per-op queue arrays
--       and checks them; drops+recreates _modify_arrays for the new signature;
--       no data movement.
--   (2) task id/queue and doc namespace/id/key_primary/key_secondary to
--       byte-order COLLATE "C" so key ranges and prefix scans match the other
--       backends; rewrites the tasks and docs tables and rebuilds their
--       keys/indexes. Transparent to clients (it changes only lexicographical
--       ordering, not data), but a heavy op -- plan a maintenance window on
--       large tables.
-- Migrations: 1.7.1 → 1.11.0 (no data movement):
--   (1) Text length CHECKs count bytes, not characters: length() becomes
--       octet_length() on tasks.id/claimant and
--       docs.namespace/id/claimant/key_primary/key_secondary. These bounds
--       budget btree index entries, which are measured in bytes, so a
--       multi-byte key could pass the old check while consuming up to 4x the
--       intended index space.
--       Collation is not involved: COLLATE "C" governs comparison, while
--       length() counts characters per the server encoding, so the columns
--       were byte-ordered but character-bounded. The new constraints are added
--       NOT VALID so a database holding legacy multi-byte values still
--       upgrades; they are enforced on every new write immediately. To check
--       and enforce the existing rows too, run during a maintenance window:
--         ALTER TABLE entroq.docs VALIDATE CONSTRAINT docs_key_primary_check;
--       (and likewise for the other six), fixing any rows it reports.
--       The same version raises the doc namespace bound from 64 to 1024 bytes.
--       A namespace is a path carrying the same /key=value marker grammar as a
--       queue name, and queue names have no length limit at all, so the 64-byte
--       cap was an asymmetry rather than a design. It also has to hold the /gc=
--       activation marker, whose RFC3339Nano form alone is 34 bytes, so a
--       namespace with a tenant path, a run id and a couple of markers reaches
--       150-250 bytes without trying. Loosening a bound cannot fail against
--       existing rows, so this constraint is added VALID.
--
--       NOTE: namespace, key_primary and key_secondary share one budget. They
--       are the columns of idx_docs_keys, and a btree index row cannot exceed
--       2704 bytes (1/3 of an 8kB page). Measured with both key columns at their
--       256 maximum and incompressible values, a namespace of 2048 bytes still
--       indexes and 2176 does not. The current allocation is
--       1024 + 256 + 256 = 1536, about 57% of the ceiling. Raising any of the
--       three means re-checking that sum; raising one far enough will break
--       inserts for the others.
--   (2) _modify_docs accepts an arrival time for inserted docs.
--   (3) Retires the unsupported direct-SQL surface, including its JSON/listing
--       wrappers, composite argument types, and PostgreSQL-specific GC helpers
--       and indexes. Backend GC continues through Go's ordinary queue listing,
--       claim, and modify operations.
-- Each block checks pg_attribute to skip on fresh installs where the column
-- is already correct, avoiding unnecessary table scans on re-runs.

DO $$
DECLARE v_not_null boolean;
BEGIN
    SELECT attnotnull INTO v_not_null FROM pg_attribute
    WHERE attrelid = 'entroq.tasks'::regclass AND attname = 'claimant';
    IF NOT v_not_null THEN
        UPDATE entroq.tasks SET claimant = '' WHERE claimant IS NULL;
        ALTER TABLE entroq.tasks ALTER COLUMN claimant SET NOT NULL;
        ALTER TABLE entroq.tasks ALTER COLUMN claimant SET DEFAULT '';
    END IF;
END $$;

DO $$
DECLARE v_not_null boolean;
BEGIN
    SELECT attnotnull INTO v_not_null FROM pg_attribute
    WHERE attrelid = 'entroq.tasks'::regclass AND attname = 'created';
    IF NOT v_not_null THEN
        UPDATE entroq.tasks SET created = modified WHERE created IS NULL;
        ALTER TABLE entroq.tasks ALTER COLUMN created SET NOT NULL;
        ALTER TABLE entroq.tasks ALTER COLUMN created SET DEFAULT now();
    END IF;
END $$;

DO $$
DECLARE v_not_null boolean;
BEGIN
    SELECT attnotnull INTO v_not_null FROM pg_attribute
    WHERE attrelid = 'entroq.docs'::regclass AND attname = 'claimant';
    IF NOT v_not_null THEN
        UPDATE entroq.docs SET claimant = '' WHERE claimant IS NULL;
        ALTER TABLE entroq.docs ALTER COLUMN claimant SET NOT NULL;
        ALTER TABLE entroq.docs ALTER COLUMN claimant SET DEFAULT '';
    END IF;
END $$;

DO $$
DECLARE v_not_null boolean;
BEGIN
    SELECT attnotnull INTO v_not_null FROM pg_attribute
    WHERE attrelid = 'entroq.docs'::regclass AND attname = 'at';
    IF NOT v_not_null THEN
        UPDATE entroq.docs SET at = created WHERE at IS NULL;
        ALTER TABLE entroq.docs ALTER COLUMN at SET NOT NULL;
        ALTER TABLE entroq.docs ALTER COLUMN at SET DEFAULT now();
    END IF;
END $$;

-- Migration detail: key/id/queue columns to byte-order "C" collation.
-- Range and prefix comparisons on these columns previously used the database's
-- default (locale) collation, which disagrees with the other backends (eqmem,
-- eqredis) and with Go's byte-order string comparison for keys containing
-- punctuation: "shard/0" is < "shard0" by byte ('/'=0x2F < '0'=0x30) but not
-- under many locales, so a byte-order-intended doc key range missed such keys.
-- "C" makes these columns byte-ordered (matching every other backend) and also
-- lets an anchored LIKE over a literal prefix use the index (a parameterized
-- pattern still cannot be folded to an index range, so prefix stats over a bind
-- parameter do not gain that particular benefit). Each block is guarded on the
-- current collation so a re-run is a no-op; ALTER COLUMN TYPE rewrites the table
-- and rebuilds the affected primary keys and indexes.
DO $$
BEGIN
    IF (SELECT co.collname FROM pg_attribute a
          JOIN pg_collation co ON co.oid = a.attcollation
         WHERE a.attrelid = 'entroq.tasks'::regclass AND a.attname = 'queue') <> 'C' THEN
        ALTER TABLE entroq.tasks
            ALTER COLUMN id    TYPE text COLLATE "C",
            ALTER COLUMN queue TYPE text COLLATE "C";
    END IF;
END $$;

DO $$
BEGIN
    IF (SELECT co.collname FROM pg_attribute a
          JOIN pg_collation co ON co.oid = a.attcollation
         WHERE a.attrelid = 'entroq.docs'::regclass AND a.attname = 'key_primary') <> 'C' THEN
        ALTER TABLE entroq.docs
            ALTER COLUMN namespace     TYPE text COLLATE "C",
            ALTER COLUMN id            TYPE text COLLATE "C",
            ALTER COLUMN key_primary   TYPE text COLLATE "C",
            ALTER COLUMN key_secondary TYPE text COLLATE "C";
    END IF;
END $$;

-- Raise the doc namespace bound to 1024 bytes (see the header). Guarded so a
-- fresh install, which already has the target constraint from the CREATE TABLE
-- above, does no work. Loosening a bound always validates against existing
-- rows, so unlike the byte-count swap below this one is added VALID.
DO $$
DECLARE v_def text;
BEGIN
    SELECT pg_get_constraintdef(oid) INTO v_def
      FROM pg_constraint
     WHERE conrelid = 'entroq.docs'::regclass AND conname = 'docs_namespace_check';
    IF v_def IS NOT NULL AND v_def NOT LIKE '%octet_length(namespace) <= 1024%' THEN
        ALTER TABLE entroq.docs DROP CONSTRAINT docs_namespace_check;
        ALTER TABLE entroq.docs ADD CONSTRAINT docs_namespace_check
            CHECK (octet_length(namespace) <= 1024);
        RAISE NOTICE 'entroq: doc namespace limit raised to 1024 bytes';
    END IF;
END $$;

-- Swap any character-counting length CHECK for a byte-counting one. Fresh
-- installs already have octet_length from the CREATE TABLE above, so the loop
-- finds nothing and this is a no-op; only a pre-1.11.0 database does work here.
-- NOT VALID keeps the upgrade from failing on a legacy row that exceeds the byte
-- bound: the rule binds every new write immediately, and the header above says
-- how to validate the existing rows when convenient.
DO $$
DECLARE
    r        record;
    v_fixed  integer := 0;
BEGIN
    FOR r IN
        SELECT c.conrelid::regclass::text AS tbl,
               c.conname                  AS name,
               pg_get_constraintdef(c.oid) AS def
          FROM pg_constraint c
         WHERE c.conrelid IN ('entroq.tasks'::regclass, 'entroq.docs'::regclass)
           AND c.contype = 'c'
           AND pg_get_constraintdef(c.oid) LIKE '%length(%'
           AND pg_get_constraintdef(c.oid) NOT LIKE '%octet_length(%'
    LOOP
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', r.tbl, r.name);
        EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I %s NOT VALID',
                       r.tbl, r.name, replace(r.def, 'length(', 'octet_length('));
        v_fixed := v_fixed + 1;
    END LOOP;
    IF v_fixed > 0 THEN
        RAISE NOTICE 'entroq: % length CHECK(s) now count bytes; they are NOT VALID until you VALIDATE CONSTRAINT them', v_fixed;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS entroq.meta (
    key   TEXT PRIMARY KEY NOT NULL,
    value TEXT NOT NULL
);

INSERT INTO entroq.meta (key, value) VALUES ('schema_version', '1.13.0')
    ON CONFLICT (key) DO UPDATE SET value = '1.13.0' WHERE entroq.meta.key = 'schema_version';
