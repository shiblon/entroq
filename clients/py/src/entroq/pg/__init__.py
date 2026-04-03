"""EntroQ PostgreSQL client.

Talks directly to a PostgreSQL database that has been initialized by the Go
eqpg backend (which installs the stored procedures modify,
try_claim_one, etc.).

Requires psycopg >= 3.
"""

import base64
import hashlib
import json
import logging
import re
import threading
import time
import uuid
from dataclasses import dataclass
from datetime import datetime, timezone, timedelta
from contextlib import contextmanager
from typing import Iterator, List, Optional, Tuple

import psycopg
from psycopg.rows import dict_row


# ---------------------------------------------------------------------------
# Data types
# ---------------------------------------------------------------------------

@dataclass
class TaskID:
    """Identifies a specific version of a task (for deletes and depends)."""
    id: str
    version: int


@dataclass
class TaskData:
    """Input spec for a new task insert."""
    queue: str
    at: Optional[datetime] = None   # None -> use database now()
    value: bytes = b''
    attempt: int = 0
    err: str = ''
    id: Optional[str] = None        # None -> auto-generate UUID


@dataclass
class TaskChange:
    """Specifies new values for an existing task (identified by id + version)."""
    id: str
    version: int
    queue: str
    at: Optional[datetime]          # required - set explicitly or copy from Task
    value: bytes = b''
    attempt: int = 0
    err: str = ''


@dataclass
class Task:
    """A task row returned from the database."""
    id: str
    version: int
    queue: str
    at: datetime
    created: Optional[datetime]
    modified: datetime
    claimant: str
    value: bytes
    claims: int
    attempt: int
    err: str

    def as_id(self) -> TaskID:
        return TaskID(self.id, self.version)

    def as_change(self, **overrides) -> TaskChange:
        """Return a TaskChange for this task with optional field overrides.

        Example:
            task.as_change(queue='other/queue', value=b'new payload')
        """
        return TaskChange(
            id=self.id,
            version=self.version,
            queue=overrides.get('queue', self.queue),
            at=overrides.get('at', self.at),
            value=overrides.get('value', self.value),
            attempt=overrides.get('attempt', self.attempt),
            err=overrides.get('err', self.err),
        )


# ---------------------------------------------------------------------------
# Dependency error
# ---------------------------------------------------------------------------

class DependencyError(Exception):
    """Raised when a Modify call fails due to dependency constraints.

    Attributes:
        missing:    task IDs that were required but not found at all.
        mismatched: task IDs found but at the wrong version.
        collisions: task IDs specified for insert that already exist.
    """

    def __init__(self, missing=(), mismatched=(), collisions=()):
        self.missing = list(missing)
        self.mismatched = list(mismatched)
        self.collisions = list(collisions)

    def __str__(self):
        return json.dumps({
            'missing': self.missing,
            'mismatched': self.mismatched,
            'collisions': self.collisions,
        })


# ---------------------------------------------------------------------------
# PostgreSQL LISTEN/NOTIFY channel name
# Mirrors pgChannelName() in backend/eqpg/pgnotify.go - must stay in sync.
# ---------------------------------------------------------------------------

_nonalnum = re.compile(r'[^a-zA-Z0-9]')


def _pg_channel_name(queue: str) -> str:
    sanitized = _nonalnum.sub('_', queue)
    if len(sanitized) + 2 <= 63:
        return 'q_' + sanitized
    h = hashlib.md5(queue.encode()).hexdigest()
    return 'q_' + sanitized[:25] + '_' + h[:8] + '_' + sanitized[-26:]


# ---------------------------------------------------------------------------
# JSONB encoding helpers for modify
# ---------------------------------------------------------------------------

def _encode_ids(items) -> str:
    return json.dumps([{'id': str(i.id), 'version': i.version} for i in items])


def _encode_inserts(items) -> str:
    out = []
    for it in items:
        obj = {
            'queue': it.queue,
            'value': base64.standard_b64encode(it.value or b'').decode(),
        }
        if it.id:
            obj['id'] = str(it.id)
        if it.at is not None:
            obj['at'] = it.at.isoformat()
        if it.attempt:
            obj['attempt'] = it.attempt
        if it.err:
            obj['err'] = it.err
        out.append(obj)
    return json.dumps(out)


def _encode_changes(items) -> str:
    out = []
    for c in items:
        obj = {
            'id': str(c.id),
            'version': c.version,
            'queue': c.queue,
            'value': base64.standard_b64encode(c.value or b'').decode(),
        }
        if c.at is not None:
            obj['at'] = c.at.isoformat()
        if c.attempt:
            obj['attempt'] = c.attempt
        if c.err:
            obj['err'] = c.err
        out.append(obj)
    return json.dumps(out)


def _row_to_task(row: dict) -> Task:
    return Task(
        id=str(row['id']),
        version=row['version'],
        queue=row['queue'],
        at=row['at'],
        created=row['created'],
        modified=row['modified'],
        claimant=str(row['claimant']) if row['claimant'] is not None else '',
        value=bytes(row['value']) if row['value'] is not None else b'',
        claims=row['claims'],
        attempt=row['attempt'],
        err=row['err'],
    )


# ---------------------------------------------------------------------------
# Transaction
# ---------------------------------------------------------------------------

class Transaction:
    """An EntroQ operation context sharing a single database transaction.

    Obtain via EntroQ.transaction(), not directly.

    The conn attribute is the underlying psycopg connection and may be used
    for arbitrary SQL within the same transaction. A DependencyError or any
    other exception raised by modify() will cause the transaction to roll back
    on context manager exit, unwinding all SQL executed within it.

    Note: claim() and try_claim() are intentionally absent. Claim before
    opening a transaction; modify (and your own SQL) inside it.
    """

    def __init__(self, conn: psycopg.Connection, claimant: str):
        self.conn = conn
        self._claimant = claimant

    def modify(
        self,
        inserts=(),
        changes=(),
        deletes=(),
        depends=(),
        unsafe_claimant_id=None,
    ) -> Tuple[List['Task'], List['Task']]:
        """Atomically apply inserts, changes, deletes, and dependency checks.

        Runs within the enclosing transaction. A DependencyError rolls back
        the entire transaction on context manager exit.
        """
        claimant = unsafe_claimant_id or self._claimant
        try:
            rows = self.conn.execute(
                '''SELECT kind, id, version, queue, at, created, modified,
                          claimant, value, claims, attempt, err
                   FROM modify(%s, %s::jsonb, %s::jsonb, %s::jsonb, %s::jsonb)''',
                (
                    claimant,
                    _encode_ids(depends),
                    _encode_ids(deletes),
                    _encode_inserts(inserts),
                    _encode_changes(changes),
                ),
            ).fetchall()
        except psycopg.DatabaseError as e:
            if e.diag.sqlstate == 'EQ001':
                detail = json.loads(e.diag.message_detail)
                raise DependencyError(
                    missing=detail.get('missing', []),
                    mismatched=detail.get('mismatched', []),
                    collisions=detail.get('collisions', []),
                ) from e
            raise

        inserted, changed = [], []
        for row in rows:
            t = _row_to_task(row)
            (inserted if row['kind'] == 'inserted' else changed).append(t)
        return inserted, changed


# ---------------------------------------------------------------------------
# Schema version check
# ---------------------------------------------------------------------------

# Must match the value in schema.sql and backend/eqpg/schema.go.
SCHEMA_VERSION = "0.10.0"


def _check_schema_version(connstr: str) -> None:
    """Verify that the database schema version matches SCHEMA_VERSION.

    Raises RuntimeError with a descriptive message on mismatch or if the
    meta table is absent (uninitialized or pre-versioning database).
    """
    with psycopg.connect(connstr, row_factory=dict_row, options="-c search_path=entroq,public") as conn:
        try:
            row = conn.execute(
                "SELECT value FROM meta WHERE key = 'schema_version'"
            ).fetchone()
        except psycopg.DatabaseError as e:
            if e.diag.sqlstate == '42P01':  # undefined_table
                raise RuntimeError(
                    "meta table not found; "
                    "initialize the database by running schema.sql"
                ) from e
            raise
        if row is None:
            raise RuntimeError(
                "schema_version not found in meta; "
                "re-run schema.sql to initialize"
            )
        stored = row['value']
        if stored != SCHEMA_VERSION:
            raise RuntimeError(
                f"schema version mismatch: database has {stored!r}, "
                f"code expects {SCHEMA_VERSION!r}; apply schema.sql to migrate, "
                f"then: UPDATE entroq.meta SET value = '{SCHEMA_VERSION}' "
                f"WHERE key = 'schema_version'"
            )


# ---------------------------------------------------------------------------
# Main client
# ---------------------------------------------------------------------------

class EntroQ:
    """EntroQ client that connects directly to PostgreSQL.

    Args:
        connstr: A libpq connection string or DSN, e.g.
                 "host=localhost dbname=entroq user=entroq password=secret"
    """

    def __init__(self, connstr: str):
        self._connstr = connstr
        self._claimant = str(uuid.uuid4())
        _check_schema_version(connstr)

    def time(self) -> datetime:
        """Return the current time according to the database."""
        with psycopg.connect(self._connstr, row_factory=dict_row, options="-c search_path=entroq,public") as conn:
            return conn.execute('SELECT now() AS t').fetchone()['t']

    def queues(self, prefix='', exact=(), limit=0) -> list:
        """Return queue stats as a list of dicts with 'name' and 'num_tasks'.

        If exact is non-empty it takes precedence over prefix.
        """
        with psycopg.connect(self._connstr, row_factory=dict_row, options="-c search_path=entroq,public") as conn:
            return conn.execute(
                'SELECT * FROM queues(%s, %s, %s)',
                (prefix, list(exact), limit),
            ).fetchall()

    def tasks(self, queue='', limit=0, omit_values=False) -> List[Task]:
        """Return tasks ordered by at. queue='' means all queues."""
        with psycopg.connect(self._connstr, row_factory=dict_row, options="-c search_path=entroq,public") as conn:
            return [_row_to_task(r) for r in conn.execute(
                'SELECT * FROM tasks(%s, %s, %s)',
                (queue, limit, omit_values),
            ).fetchall()]

    def try_claim(self, queue, duration_ms=30000) -> Optional[Task]:
        """Try to claim one task from queue (str or list of str).

        Returns None if no task is available in any of the given queues.
        """
        queues = queue if isinstance(queue, (list, tuple)) else [queue]
        with psycopg.connect(self._connstr, row_factory=dict_row, options="-c search_path=entroq,public") as conn:
            rows = conn.execute(
                "SELECT * FROM try_claim(%s, %s, %s::interval)",
                (queues, self._claimant, f'{duration_ms} milliseconds'),
            ).fetchall()
        return _row_to_task(rows[0]) if rows else None

    def claim(self, queue, duration_ms=30000, poll_ms=5000, timeout_s=None,
              poll_only=False) -> Task:
        """Block until a task is available in queue (str or list), then claim it.

        By default uses PostgreSQL LISTEN/NOTIFY to wake up promptly when a
        task arrives, falling back to poll_ms-interval polling if no
        notification arrives.

        Connection pool compatibility:
            The LISTEN/NOTIFY path holds a persistent direct connection for
            the duration of the call. This is incompatible with
            transaction-mode connection poolers (e.g. PgBouncer in transaction
            mode), which reassign server connections between transactions and
            silently drop LISTEN registrations.

            Set poll_only=True to disable LISTEN/NOTIFY and poll instead.
            This is safe behind any pooler. At scale, polling is perfectly
            adequate -- with many workers it is highly likely that one will
            poll at just the right moment, making the occasional missed
            notification irrelevant.

        Args:
            poll_ms:   interval between poll attempts, in milliseconds.
            timeout_s: maximum seconds to wait, or None to wait forever.
                       Raises TimeoutError if the timeout elapses.
            poll_only: skip LISTEN/NOTIFY and use pure polling instead.
                       Safe behind transaction-mode connection poolers.

        If the LISTEN connection drops (network error, server restart), an
        exception is raised rather than silently retrying.
        """
        deadline = None if timeout_s is None else time.time() + timeout_s

        def _wait(notify):
            """Try to claim; if nothing available, wait then return False, or
            raise TimeoutError if the deadline has passed. Returns True when a
            task is claimed."""
            t = self.try_claim(queue, duration_ms)
            if t is not None:
                return t
            if deadline is not None and time.time() >= deadline:
                raise TimeoutError(f'claim timed out after {timeout_s}s')
            wait = poll_ms / 1000 if deadline is None else min(poll_ms / 1000, deadline - time.time())
            notify(wait)
            return None

        if poll_only:
            while True:
                t = _wait(time.sleep)
                if t is not None:
                    return t

        queues = queue if isinstance(queue, (list, tuple)) else [queue]
        channels = [_pg_channel_name(q) for q in queues]
        with psycopg.connect(self._connstr, options="-c search_path=entroq,public") as lconn:
            lconn.autocommit = True
            for ch in channels:
                # LISTEN does not support parameter placeholders, so the
                # channel name is interpolated directly. This is safe because
                # _pg_channel_name guarantees output contains only [a-zA-Z0-9_].
                lconn.execute(f'LISTEN "{ch}"')

            def _notify_wait(wait):
                for _ in lconn.notifies(timeout=wait):
                    break   # one notification is enough; retry the claim

            while True:
                t = _wait(_notify_wait)
                if t is not None:
                    return t

    @contextmanager
    def transaction(self):
        """Context manager that runs EntroQ operations and user SQL in one transaction.

        Yields a Transaction whose .conn is the underlying psycopg connection,
        available for arbitrary SQL alongside entroq operations. Commits on
        clean exit; rolls back on any exception including DependencyError.

        Usage::

            task = eq.claim(queue)
            with eq.transaction() as txn:
                txn.conn.execute("UPDATE my_table SET ... WHERE ...", (...,))
                txn.modify(deletes=[task.as_id()], inserts=[TaskData(...)])

        Note: claim() and try_claim() are intentionally absent from Transaction.
        Always claim before opening a transaction.
        """
        with psycopg.connect(self._connstr, row_factory=dict_row, options="-c search_path=entroq,public") as conn:
            yield Transaction(conn, self._claimant)

    def modify(
        self,
        inserts=(),
        changes=(),
        deletes=(),
        depends=(),
        unsafe_claimant_id=None,
    ) -> Tuple[List[Task], List[Task]]:
        """Atomically apply inserts, changes, deletes, and dependency checks.

        Note that if you want to run modify in a shared transaction with other
        database operations, use the transaction() context manager and call
        Transaction.modify() instead of this method, which runs its own
        transaction.  See Transaction.modify() docs for details.

        Args:
            inserts:  iterable of TaskData.
            changes:  iterable of TaskChange.
            deletes:  iterable of TaskID (or Task - anything with .id/.version).
            depends:  iterable of TaskID (or Task) that must exist at their version.
            unsafe_claimant_id: override the claimant UUID (use with care).

        Returns:
            (inserted, changed) - lists of Task.

        Raises:
            DependencyError if any dependency constraint is violated.
        """
        with self.transaction() as txn:
            return txn.modify(
                inserts=inserts,
                changes=changes,
                deletes=deletes,
                depends=depends,
                unsafe_claimant_id=unsafe_claimant_id,
            )

    def delete(self, task):
        """Delete a single task (Task or TaskID)."""
        self.modify(deletes=[task])

    def renew_for(self, task: Task, duration=30) -> Task:
        """Extend a task's claim expiry by duration seconds."""
        at = datetime.now(tz=timezone.utc) + timedelta(seconds=duration)
        _, changed = self.modify(changes=[task.as_change(at=at)])
        return changed[0]

    @contextmanager
    def renewing(self, task: Task, duration=30):
        """Context manager that renews a task claim in the background.

        Yields a callable that returns the current (most recently renewed)
        version of the task. On exit, renewal stops and the callable returns
        the final stable version.

        Inside the `with` block, renewal is running and the task version may
        advance. After the block, the version is stable and safe to use for
        modifications up to the expiration of the task's lease, which should be
        at least half the lease renewal time.
        
        Example::

            with eq.renewing(task, duration=30) as current_task:
                result = do_heavy_work()
            # renewal stopped; current_task() is now stable
            eq.modify(deletes=[current_task().as_id()], ...)
        """
        exit_event = threading.Event()
        lock = threading.Lock()
        current = [task]

        def latest():
            with lock:
                return current[0]

        def _renewer():
            exit_event.wait(duration / 2)
            while not exit_event.is_set():
                try:
                    t = self.renew_for(current[0], duration=duration)
                    with lock:
                        current[0] = t
                except Exception:
                    logging.exception('renew_for failed')
                exit_event.wait(duration / 2)

        t = threading.Thread(target=_renewer, daemon=True)
        t.start()
        try:
            yield latest
        finally:
            exit_event.set()
            t.join()

    def pop_all(self, queue, force=False) -> Iterator[Task]:
        """Claim and delete every task in queue, yielding each one.

        Args:
            force: if True, delete tasks without claiming first by spoofing
                   each task's current claimant. Use with caution.
        """
        if force:
            for task in self.tasks(queue=queue):
                self.modify(deletes=[task.as_id()], unsafe_claimant_id=task.claimant)
                yield task
            return

        while True:
            task = self.try_claim(queue)
            if task is None:
                return
            self.modify(deletes=[task.as_id()])
            yield task


# ---------------------------------------------------------------------------
# Worker
# ---------------------------------------------------------------------------

class StopWorker(Exception):
    """Raise from a work function to stop the worker loop cleanly."""


class EQWorker:
    """Claims tasks from a queue and dispatches them to a handler function.

    Example::

        eq = EntroQ('host=localhost dbname=entroq')

        def handle(renew, finalize):
            with renew as task:
                result = do_heavy_work(task)

            with finalize as task:
                # task now has a stable version, finish up
                with eq.transaction() as txn:
                    txn.conn.execute("UPDATE my_table SET status='done' WHERE id = %s",
                                     (stable_task.id,))
                    txn.modify(deletes=[stable_task.as_id()],
                               inserts=[TaskData(queue='/done', value=result)])

        w = EQWorker(eq)
        w.work('/jobs', handle)

    do_func receives:
        renew: a context manager that provides the claimed initial task and
            renews it in the background.
        finalize: a context manager that provides the final task after
            background renewal has stopped. This must not be used before renew.
            Here you can do final modifications to the task and do
            in-transaction work. It is not required to make use of finalize.
            Skipping it allows the task to expire and be picked up by another
            work cycle.
    """

    def __init__(self, eq: EntroQ):
        self.eq = eq

    def work(self, queue, do_func, claim_duration=30, poll_only=False):
        """Loop forever: claim a task and call do_func(task, finalize) with renewal.

        Args:
            poll_only: passed through to claim(). Set True when running behind
                       a transaction-mode connection pooler.
        """
        while True:
            try:
                task = self.eq.claim(queue, duration_ms=1000 * claim_duration,
                                     poll_only=poll_only)
                final_task = [None]
                @contextmanager
                def renew():
                    with self.eq.renewing(task, duration=claim_duration) as current_task:
                        yield current_task()
                    final_task[0] = current_task()

                @contextmanager
                def finalize():
                    if final_task[0] is None:
                        raise RuntimeError('finalize called before renew')
                    yield final_task[0]

                do_func(renew(), finalize())
            except StopWorker:
                return
            except DependencyError as e:
                logging.warning('Worker continuing after dependency error: %s', e)
            except TimeoutError as e:
                logging.warning('Worker continuing after timeout: %s', e)
