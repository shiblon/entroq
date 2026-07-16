-- EntroQ psql worker example
--
-- Demonstrates the full task lifecycle from a plain psql session: insert, claim,
-- complete, and retry-on-error. This is the direct-SQL path -- EntroQ's
-- PostgreSQL backend is pure stored procedures, so a worker in any language (or a
-- shell script) can drive it with nothing but a psql connection: no EntroQ
-- server, gRPC, or client library required.
--
-- Prerequisites:
--   psql -d mydb -f path/to/schema.sql   -- apply the schema once
--   psql -d mydb -f worker_example.sql   -- run this file
--
-- Or interactively:
--   psql -d mydb
--   \i path/to/schema.sql
--   \i worker_example.sql
--
-- A task's value is JSON (the tasks.value column is jsonb) -- a string, number,
-- object, or array, passed and returned as-is with no encoding.
--
-- All mutations go through entroq.modify (which takes JSONB arrays of
-- operations); claims go through entroq.try_claim.


-- ============================================================
-- 1. Insert tasks
-- ============================================================

-- Insert a single task with an auto-generated ID. 'queue' is required; all other
-- fields are optional. Omitting 'at' schedules it for immediate availability.
SELECT kind, id, version, queue, value
FROM entroq.modify(
    'my-worker',
    p_inserts := '[
        {"queue": "/jobs/email", "value": "hello world"}
    ]'::jsonb
);

-- Insert several tasks at once. All inserts in one entroq.modify call are atomic
-- -- either all succeed or none do. Values can be structured JSON.
SELECT kind, id, version, queue, value
FROM entroq.modify(
    'my-worker',
    p_inserts := '[
        {"queue": "/jobs/email", "value": {"to": "a@example.com"}},
        {"queue": "/jobs/email", "value": {"to": "b@example.com"}},
        {"queue": "/jobs/sms",   "value": {"to": "+15551234"}}
    ]'::jsonb
);

-- Insert a task with an explicit ID and a future availability time. Useful when
-- you need to reference the ID before it exists, or to schedule work for later.
SELECT kind, id, version, queue
FROM entroq.modify(
    'my-worker',
    p_inserts := '[
        {
            "id":    "my-known-id-001",
            "queue": "/jobs/email",
            "at":    "2099-01-01T00:00:00Z",
            "value": "future job"
        }
    ]'::jsonb
);


-- ============================================================
-- 2. Inspect queues and tasks
-- ============================================================

-- List all queues and their task counts.
SELECT name, num_tasks FROM entroq.queues();

-- List queues matching a prefix.
SELECT name, num_tasks FROM entroq.queues(p_prefix := '/jobs/');

-- List tasks in a queue, oldest-first.
SELECT id, version, at, value
FROM entroq.tasks(p_queue := '/jobs/email');

-- List tasks across all queues (the default p_queue='' means all).
SELECT queue, id, version, at
FROM entroq.tasks()
ORDER BY at;


-- ============================================================
-- 3. Claim a task
-- ============================================================

-- entroq.try_claim atomically claims one available task from any of the given
-- queues (an array) for the given duration. It returns zero rows if nothing is
-- available right now -- the caller should poll, or use LISTEN/NOTIFY (below).
--
-- The claimed task's 'at' is set to now() + duration and its version bumps. The
-- worker holding it must complete or renew it before that time, or another
-- worker may claim it.

SELECT id, version, queue, claimant, value, at AS lease_expires
FROM entroq.try_claim(
    ARRAY['/jobs/email'],  -- queues to claim from (only one task, from one queue)
    'worker-abc',          -- claimant ID (any unique string per worker)
    '30 seconds'           -- lease duration
);

-- In a psql script, capture the claimed task with \gset so later statements can
-- reference :task_id, :task_version, and :'task_queue':
--
--   SELECT id, version, queue
--   FROM entroq.try_claim(ARRAY['/jobs/email'], 'worker-abc', '30 seconds')
--   \gset task_


-- ============================================================
-- 4. Complete a task (delete after successful processing)
-- ============================================================

-- After processing, delete the task by id + version + queue. There are two
-- safety checks: the VERSION guards against a concurrent re-claim, and the QUEUE
-- is part of the modify key -- a delete must name the queue the task currently
-- occupies, and the backend never substitutes it. A mismatch on either raises
-- SQLSTATE EQ001 with a JSON detail describing the failed dependency. (This
-- queue check is what makes queue-based authorization unbypassable: you cannot
-- reach a task by misdeclaring where it lives.)
--
-- Replace <id> and <version> with values from your claim above.

SELECT kind, id, version
FROM entroq.modify(
    'worker-abc',
    p_deletes := '[{"id": "<id>", "version": <version>, "queue": "/jobs/email"}]'::jsonb
);
-- Deletes produce no output rows on success.


-- ============================================================
-- 5. Retry on error (re-enqueue with attempt counter and error string)
-- ============================================================

-- On failure, delete the claimed task and re-insert it in the same atomic
-- entroq.modify call. Incrementing 'attempt' and setting 'err' preserves the
-- failure history. The delete + insert happen together: if the delete's
-- version/queue check fails, neither operation takes effect.

SELECT kind, id, version, queue, attempt, err, value
FROM entroq.modify(
    'worker-abc',
    p_deletes := '[{"id": "<id>", "version": <version>, "queue": "/jobs/email"}]'::jsonb,
    p_inserts := '[{
        "queue":   "/jobs/email",
        "value":   "hello world",
        "attempt": 1,
        "err":     "SMTP connection refused"
    }]'::jsonb
);
-- The new task gets a fresh auto-generated ID and version 0. 'at' is omitted so
-- it is available immediately; add an "at" with a future timestamp for backoff.


-- ============================================================
-- 6. LISTEN/NOTIFY for efficient polling (optional)
-- ============================================================

-- Instead of spinning in a tight claim loop, listen for a wakeup. Each queue has
-- a notification channel; entroq.channel_name derives it from the queue name.

SELECT entroq.channel_name('/jobs/email');
-- e.g. "q__jobs_email"

-- Notifications are emitted by entroq.notify_ready_queues(), which sends a
-- pg_notify on the channel of every queue that has a task ready now (at <=
-- now()) and returns the queues it notified. The EntroQ service calls it on its
-- heartbeat; a pure-SQL deployment calls it itself (after inserts, or on a
-- timer):
SELECT entroq.notify_ready_queues();

-- In an interactive psql session, LISTEN on the channel, then drive claims in
-- response to the async notifications psql prints:
--
--   LISTEN q__jobs_email;
--   SELECT entroq.notify_ready_queues();   -- or let the service do it
--   -- psql prints: Asynchronous notification "q__jobs_email" received ...
--
-- In a shell script, a poll loop is simplest:
--
--   while true; do
--     psql "$DSN" -c "
--       SELECT id, version, value
--       FROM entroq.try_claim(ARRAY['/jobs/email'], 'worker-'$$, '30 seconds');
--     "
--     sleep 1
--   done
--
-- For true async wakeups in code, use a client library that exposes LISTEN/
-- NOTIFY (Python asyncpg, Go lib/pq or pgx, etc.).
