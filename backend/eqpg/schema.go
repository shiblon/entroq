package eqpg

import (
	"context"
	"fmt"
	"hash/crc32"
	"strings"
	"unicode"
)

// pgChannelName converts a queue name into a PostgreSQL LISTEN/NOTIFY channel
// name within the 63-byte identifier limit. Uses the full sanitized name when
// it fits; otherwise sandwiches a CRC32 of the original name between a prefix
// and suffix to aid debuggability.
func pgChannelName(queue string) string {
	var b strings.Builder
	for _, r := range queue {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	sanitized := b.String()

	// If there's room, just use the sanitized name.
	if 2+len(sanitized) <= 63 { // "q_" prefix = 2 bytes
		return "q_" + sanitized
	}

	// If there isn't room, sandwich a CRC32 of the middle into the name, but
	// keep a prefix and suffix to aid debugging.
	const (
		prefixLen = 25
		suffixLen = 26
	)
	crc := fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(queue)))
	runes := []rune(sanitized)
	return fmt.Sprintf("q_%s_%s_%s",
		string(runes[:prefixLen]),
		crc,
		string(runes[len(runes)-suffixLen:]),
	)
}

// schemaSteps are the idempotent DDL statements that initialize (or upgrade)
// the database. Steps run in order; column migrations must precede the CREATE
// TABLE so that existing databases gain the new columns before the table
// creation no-ops on them.
var schemaSteps = []struct {
	name string
	sql  string
}{
	// pgcrypto provides gen_random_uuid() on PostgreSQL < 13.
	{name: "extension pgcrypto", sql: `CREATE EXTENSION IF NOT EXISTS pgcrypto`},
	// Column migrations for databases predating the claims/attempt/err fields.
	{
		name: "add claims column",
		sql:  `ALTER TABLE IF EXISTS tasks ADD COLUMN IF NOT EXISTS claims INTEGER NOT NULL DEFAULT 0`,
	},
	{
		name: "add attempt column",
		sql:  `ALTER TABLE IF EXISTS tasks ADD COLUMN IF NOT EXISTS attempt INTEGER NOT NULL DEFAULT 0`,
	},
	{
		name: "add err column",
		sql:  `ALTER TABLE IF EXISTS tasks ADD COLUMN IF NOT EXISTS err TEXT NOT NULL DEFAULT ''`,
	},
	// Core table.
	{
		name: "create tasks table",
		sql: `CREATE TABLE IF NOT EXISTS tasks (
			id       UUID                     PRIMARY KEY NOT NULL,
			version  INTEGER                  NOT NULL DEFAULT 0,
			queue    TEXT                     NOT NULL DEFAULT '',
			at       TIMESTAMP WITH TIME ZONE NOT NULL,
			created  TIMESTAMP WITH TIME ZONE,
			modified TIMESTAMP WITH TIME ZONE NOT NULL,
			claimant UUID,
			value    BYTEA,
			claims   INTEGER                  NOT NULL DEFAULT 0,
			attempt  INTEGER                  NOT NULL DEFAULT 0,
			err      TEXT                     NOT NULL DEFAULT ''
		)`,
	},
	// Indexes.
	{name: "index byID", sql: `CREATE INDEX IF NOT EXISTS byID      ON tasks (id)`},
	{name: "index byVersion", sql: `CREATE INDEX IF NOT EXISTS byVersion ON tasks (version)`},
	{name: "index byQueue", sql: `CREATE INDEX IF NOT EXISTS byQueue   ON tasks (queue)`},
	{name: "index byQueueAt", sql: `CREATE INDEX IF NOT EXISTS byQueueAt ON tasks (queue, at)`},
	// Composite types used by stored procedures.
	// PostgreSQL has no CREATE TYPE IF NOT EXISTS, so we use DO/EXCEPTION blocks.
	{
		name: "type entroq_task_id",
		sql: `
			DO $$ BEGIN
				CREATE TYPE entroq_task_id AS (
					id      uuid,
					version integer
				);
			EXCEPTION WHEN duplicate_object THEN NULL;
			END $$`,
	},
	{
		// entroq_task_arg is used for both inserts and changes. For inserts,
		// the version field is ignored. NULL id means auto-generate; NULL at
		// means use the current transaction time.
		name: "type entroq_task_arg",
		sql: `
			DO $$ BEGIN
				CREATE TYPE entroq_task_arg AS (
					id      uuid,
					version integer,
					queue   text,
					at      timestamptz,
					value   bytea,
					attempt integer,
					err     text
				);
			EXCEPTION WHEN duplicate_object THEN NULL;
			END $$`,
	},
	// Stored procedures. entroq_try_claim_bucket must precede entroq_try_claim_one.
	{
		// entroq_try_claim_bucket claims one available task whose ID falls in the
		// given bucket (get_byte(uuid_send(id), 15) % p_num_buckets = p_bucket).
		// When p_num_buckets=1 the filter is a no-op (x%1=0 always), giving an
		// unfiltered claim. Returns the claimed task row, or no rows if none available.
		name: "function entroq_try_claim_bucket",
		sql: `
			CREATE OR REPLACE FUNCTION entroq_try_claim_bucket(
				p_queue       text,
				p_claimant    uuid,
				p_duration    interval,
				p_now         timestamptz,
				p_num_buckets integer,
				p_bucket      integer
			) RETURNS TABLE(
				id       uuid,
				version  integer,
				queue    text,
				at       timestamptz,
				created  timestamptz,
				modified timestamptz,
				claimant uuid,
				value    bytea,
				claims   integer,
				attempt  integer,
				err      text
			) LANGUAGE plpgsql AS $$
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
							get_byte(uuid_send(t2.id), 15) % p_num_buckets = p_bucket
						FOR UPDATE SKIP LOCKED
						LIMIT 1
					)
					RETURNING
						tasks.id, tasks.version, tasks.queue, tasks.at,
						tasks.created, tasks.modified, tasks.claimant,
						tasks.value, tasks.claims, tasks.attempt, tasks.err;
			END;
			$$`,
	},
	{
		// entroq_try_claim_one selects a bucket based on available task count and
		// calls entroq_try_claim_bucket. Falls back to an unfiltered claim
		// (p_num_buckets=1) if the chosen bucket is empty.
		//
		// Use of SKIP LOCKED:
		// - https://dba.stackexchange.com/questions/69471/postgres-update-limit-1
		// - https://blog.2ndquadrant.com/what-is-select-skip-locked-for-in-postgresql-9-5/
		name: "function entroq_try_claim_one",
		sql: `
			CREATE OR REPLACE FUNCTION entroq_try_claim_one(
				p_queue     text,
				p_claimant  uuid,
				p_duration  interval
			) RETURNS TABLE(
				id       uuid,
				version  integer,
				queue    text,
				at       timestamptz,
				created  timestamptz,
				modified timestamptz,
				claimant uuid,
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
				-- Count available tasks; relies on the (queue, at) index.
				SELECT COUNT(*) INTO v_n_avail
				FROM tasks t
				WHERE t.queue = p_queue AND t.at <= v_now;

				IF v_n_avail = 0 THEN
					RETURN;
				END IF;

				-- Buckets are used as a cheap "ORDER BY random()" without resorting to a full
				-- table scan. Lower order bits of the task ID are used to dynamically bucket
				-- them, and a random bucket is selected before querying. This mitigates
				-- worker starvation without the computational burden of perfect randomness.
				-- 1 bucket for a single task (no-op filter), 2 for small queues, 4 for larger.
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

				-- Fallback: bucket was empty; retry with no-op filter (num_buckets=1).
				RETURN QUERY
					SELECT * FROM entroq_try_claim_bucket(
						p_queue, p_claimant, p_duration, v_now, 1, 0
					);
			END;
			$$`,
	},
	{
		// entroq_modify atomically applies inserts, changes, deletes, and
		// dependency checks. Uses parallel arrays instead of composite type
		// parameters to avoid composite literal encoding complexity (especially
		// bytea) from the Go caller.
		//
		// Raises SQLSTATE EQ001 with a JSON detail on any dependency problem.
		// The detail has three arrays:
		//   'missing':    must-exist deps not found at all
		//   'mismatched': must-exist deps found at wrong version
		//   'collisions': explicit insert IDs that already exist
		// All three are checked before raising, so the caller sees all problems
		// at once.
		//
		// Returns tagged rows: kind='inserted' or kind='changed'.
		// Deleted tasks produce no output rows.
		//
		// Insert sentinel: zero UUID in p_ins_ids means auto-generate.
		// Timestamp sentinel: Go's zero time ('0001-01-01 00:00:00+00') means use now().
		name: "function entroq_modify",
		sql: `
CREATE OR REPLACE FUNCTION entroq_modify(
	p_claimant     uuid,
	-- depends: must exist at the given version
	p_dep_ids      uuid[],
	p_dep_vers     integer[],
	-- deletes: must exist at the given version, then removed
	p_del_ids      uuid[],
	p_del_vers     integer[],
	-- inserts: zero UUID = auto-generate, zero timestamptz = now()
	p_ins_ids      uuid[],
	p_ins_queues   text[],
	p_ins_ats      timestamptz[],
	p_ins_values   bytea[],
	p_ins_attempts integer[],
	p_ins_errs     text[],
	-- changes: must exist at the given version, then updated
	p_chg_ids      uuid[],
	p_chg_vers     integer[],
	p_chg_queues   text[],
	p_chg_ats      timestamptz[],
	p_chg_values   bytea[],
	p_chg_attempts integer[],
	p_chg_errs     text[]
) RETURNS TABLE(
	kind     text,
	id       uuid,
	version  integer,
	queue    text,
	at       timestamptz,
	created  timestamptz,
	modified timestamptz,
	claimant uuid,
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
		SELECT * FROM unnest(coalesce(p_dep_ids, '{}'::uuid[]), coalesce(p_dep_vers, '{}'::integer[]))
		UNION ALL
		SELECT * FROM unnest(coalesce(p_del_ids, '{}'::uuid[]), coalesce(p_del_vers, '{}'::integer[]))
		UNION ALL
		SELECT * FROM unnest(coalesce(p_chg_ids, '{}'::uuid[]), coalesce(p_chg_vers, '{}'::integer[]))
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
	FROM unnest(coalesce(p_ins_ids, '{}'::uuid[])) AS i(chk_id)
	JOIN tasks t ON t.id = i.chk_id
	WHERE i.chk_id != '00000000-0000-0000-0000-000000000000';

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
	USING unnest(coalesce(p_del_ids, '{}'::uuid[]), coalesce(p_del_vers, '{}'::integer[])) AS d(del_id, del_ver)
	WHERE tasks.id = d.del_id AND tasks.version = d.del_ver;

	-- Inserts: zero UUID = auto-generate; Go zero time = use v_now.
	-- CTE wraps the INSERT so RETURNING * is unambiguous; the outer SELECT
	-- uses alias r.col to avoid RETURNS TABLE OUT-parameter shadowing.
	RETURN QUERY
		WITH r AS (
			INSERT INTO tasks (id, version, queue, at, claimant, value, created, modified, attempt, err)
			SELECT
				CASE WHEN ins_id = '00000000-0000-0000-0000-000000000000' THEN gen_random_uuid() ELSE ins_id END,
				0,
				ins_queue,
				CASE WHEN ins_at = '0001-01-01 00:00:00+00'::timestamptz THEN v_now ELSE ins_at END,
				p_claimant,
				ins_value,
				v_now, v_now,
				ins_attempt, ins_err
			FROM unnest(
				coalesce(p_ins_ids,      '{}'::uuid[]),
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
				coalesce(p_chg_ids,      '{}'::uuid[]),
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
$$`,
	},
}

// initDB sets up the database to have the appropriate tables and necessary
// extensions to work as a task queue backend. All steps are idempotent.
func (b *backend) initDB(ctx context.Context) error {
	for _, step := range schemaSteps {
		if _, err := b.db.ExecContext(ctx, step.sql); err != nil {
			return fmt.Errorf("initDB %s: %w", step.name, err)
		}
	}
	return nil
}
