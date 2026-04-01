-- EntroQ psql worker example
--
-- Demonstrates the full task lifecycle from a plain psql session:
-- setup, insert, claim, complete, and retry-on-error.
--
-- Prerequisites:
--   psql -d mydb -f path/to/schema.sql   -- apply schema once
--   psql -d mydb -f worker_example.sql   -- run this file
--
-- Or interactively:
--   psql -d mydb
--   \i path/to/schema.sql
--   \i worker_example.sql
--
-- Values are bytea. Text payloads are encoded as UTF-8:
--   encode('hello'::bytea, 'base64')   -- text  -> base64 JSON string
--   convert_from(value, 'UTF8')        -- bytea -> text for display
--
-- All operations go through entroq_modify (JSONB) or entroq_try_claim_one.


-- ============================================================
-- 1. Insert tasks
-- ============================================================

-- Insert a single task with an auto-generated ID.
-- 'queue' is required; all other fields are optional.
-- Omitting 'at' schedules the task for immediate availability.
SELECT kind, id, version, queue, convert_from(value, 'UTF8') AS payload
FROM entroq_modify(
    'my-worker',
    p_inserts := '[
        {"queue": "/jobs/email", "value": "aGVsbG8gd29ybGQ="}
    ]'::jsonb
);
-- aGVsbG8gd29ybGQ= is base64 for "hello world"
-- encode(''hello world''::bytea, ''base64'') produces it

-- Insert several tasks at once. All inserts in one entroq_modify call
-- are atomic -- either all succeed or none do.
SELECT kind, id, version, queue, convert_from(value, 'UTF8') AS payload
FROM entroq_modify(
    'my-worker',
    p_inserts := '[
        {"queue": "/jobs/email", "value": "am9iIG9uZQ=="},
        {"queue": "/jobs/email", "value": "am9iIHR3bw=="},
        {"queue": "/jobs/sms",   "value": "c21zIGpvYg=="}
    ]'::jsonb
);

-- Insert a task with an explicit ID and a future availability time.
-- Useful when you need to reference the ID before it exists, or to
-- schedule work for later.
SELECT kind, id, version, queue
FROM entroq_modify(
    'my-worker',
    p_inserts := '[
        {
            "id":    "my-known-id-001",
            "queue": "/jobs/email",
            "at":    "2099-01-01T00:00:00Z",
            "value": "ZnV0dXJlIGpvYg=="
        }
    ]'::jsonb
);


-- ============================================================
-- 2. Inspect queues and tasks
-- ============================================================

-- List all queues and their task counts.
SELECT name, num_tasks FROM entroq_queues();

-- List queues matching a prefix.
SELECT name, num_tasks FROM entroq_queues(p_prefix := '/jobs/');

-- List tasks in a queue, oldest-first.
SELECT id, version, at, convert_from(value, 'UTF8') AS payload
FROM entroq_tasks(p_queue := '/jobs/email');

-- List tasks across all queues (p_queue='' means all).
SELECT queue, id, version, at
FROM entroq_tasks()
ORDER BY at;


-- ============================================================
-- 3. Claim a task
-- ============================================================

-- entroq_try_claim_one atomically claims one available task from the
-- given queue for the given duration. Returns zero rows if no task is
-- available right now (caller should poll or use LISTEN/NOTIFY).
--
-- The claimed task's 'at' is set to now() + duration. Any worker that
-- holds the task must complete or renew it before that time, or another
-- worker may claim it.

SELECT id, version, queue, claimant,
       convert_from(value, 'UTF8') AS payload, at AS lease_expires
FROM entroq_try_claim_one(
    '/jobs/email',  -- queue
    'worker-abc',   -- claimant ID (any unique string per worker)
    '30 seconds'    -- lease duration
);

-- In a psql script, use \gset to capture the claimed task into variables
-- so subsequent statements can reference :task_id and :task_version.
--
-- Example (substitute real id/version from the claim above):
--
--   SELECT id, version
--   FROM entroq_try_claim_one('/jobs/email', 'worker-abc', '30 seconds')
--   \gset task_
--
-- :task_id and :task_version are then available below.


-- ============================================================
-- 4. Complete a task (delete after successful processing)
-- ============================================================

-- After processing, delete the task by id + version. The version check
-- is the safety mechanism: if the task was claimed by another worker
-- (i.e., its version incremented), entroq_modify raises SQLSTATE EQ001
-- with a JSON detail describing the mismatched dependency.
--
-- Replace <id> and <version> with values from your claim above.

SELECT kind, id, version
FROM entroq_modify(
    'worker-abc',
    p_deletes := '[{"id": "<id>", "version": <version>}]'::jsonb
);
-- Returns zero rows on success (deletes produce no output rows).
-- Raises EQ001 if the version no longer matches (task was re-claimed
-- or modified by someone else since you claimed it).


-- ============================================================
-- 5. Retry on error (re-enqueue with attempt counter and error string)
-- ============================================================

-- On failure, delete the claimed task and re-insert it in the same
-- atomic modify call. Incrementing 'attempt' and setting 'err' lets
-- downstream workers or operators see the failure history.
--
-- The delete + insert happen together: if the version check fails
-- (another worker claimed it), neither operation takes effect.

SELECT kind, id, version, queue,
       attempt, err, convert_from(value, 'UTF8') AS payload
FROM entroq_modify(
    'worker-abc',
    p_deletes := '[{"id": "<id>", "version": <version>}]'::jsonb,
    p_inserts := '[{
        "queue":   "/jobs/email",
        "value":   "aGVsbG8gd29ybGQ=",
        "attempt": 1,
        "err":     "SMTP connection refused"
    }]'::jsonb
);
-- The new task gets a fresh auto-generated ID and version 0.
-- 'at' is omitted so it is available immediately for retry.
-- Add an "at" field with a future timestamp to implement backoff.


-- ============================================================
-- 6. LISTEN/NOTIFY for efficient polling (optional)
-- ============================================================

-- The trigger entroq_task_notify fires pg_notify on the queue's channel
-- whenever a task becomes immediately available (at <= now()).
-- entroq_channel_name() derives the channel name from the queue name.

SELECT entroq_channel_name('/jobs/email');
-- e.g. "q__jobs_email"

-- In an interactive psql session, LISTEN on that channel before polling:
--
--   LISTEN q__jobs_email;
--
-- psql will print an asynchronous notification line whenever a task
-- arrives, so you can run entroq_try_claim_one in response rather than
-- spinning in a tight loop.
--
-- In a bash script, pg_isready + a sleep loop is simpler:
--
--   while true; do
--     psql "$DSN" -c "
--       SELECT id, version, convert_from(value,'UTF8') AS payload
--       FROM entroq_try_claim_one('/jobs/email', 'worker-$$', '30 seconds');
--     "
--     sleep 1
--   done
--
-- For true async notification in a script, consider psql's --single-transaction
-- mode with LISTEN, or use a language client (Python asyncpg, Go lib/pq, etc.)
-- that exposes pg_notify callbacks.
