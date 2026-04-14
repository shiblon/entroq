"""EntroQ PostgreSQL client.

Talks directly to a PostgreSQL database that has been initialized by the Go
eqpg backend (which installs the stored procedures modify,
try_claim_one, etc.).

Requires psycopg >= 3.
"""

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
from typing import Iterator, List, Optional, Tuple, Union

import psycopg
from psycopg.rows import dict_row

from ..types import Task, TaskData, TaskChange, TaskID, DependencyError
from ..base import EntroQBase
from ..worker import renewing as common_renewing, EntroQWorker as common_worker, StopWorker


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
            'value': it.value,
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
            'value': c.value,
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
        value=row['value'],
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
SCHEMA_VERSION = "0.12.0"

_INIT_HINT = (
    "Initialize the database with:\n\n"
    "  docker run --rm shiblon/entroq:1 schema init --db YOUR_DSN\n\n"
    "where YOUR_DSN is a libpq connection string, e.g.:\n"
    "  'host=localhost dbname=entroq user=entroq password=secret'"
)


def _check_schema_version(connstr: str) -> None:
    """Verify that the database schema version matches SCHEMA_VERSION.

    Raises RuntimeError with an actionable message on mismatch or if the
    schema has not been initialized.
    """
    with psycopg.connect(connstr, row_factory=dict_row, options="-c search_path=entroq,public") as conn:
        try:
            row = conn.execute(
                "SELECT value FROM meta WHERE key = 'schema_version'"
            ).fetchone()
        except psycopg.DatabaseError as e:
            if e.diag.sqlstate == '42P01':  # undefined_table
                raise RuntimeError(
                    f"EntroQ schema not found -- database has not been initialized.\n"
                    f"{_INIT_HINT}"
                ) from e
            raise
        if row is None:
            raise RuntimeError(
                f"EntroQ schema not initialized (schema_version missing).\n"
                f"{_INIT_HINT}"
            )
        stored = row['value']
        if stored != SCHEMA_VERSION:
            raise RuntimeError(
                f"EntroQ schema version mismatch: database has {stored!r}, "
                f"this client expects {SCHEMA_VERSION!r}.\n"
                f"{_INIT_HINT}"
            )


# ---------------------------------------------------------------------------
# Main client
# ---------------------------------------------------------------------------

class EntroQ(EntroQBase):
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

    def queues(self, prefix: str = '', exact: List[str] = (), limit: int = 0) -> List[dict]:
        """Return queue stats as a list of dicts with 'name' and 'num_tasks'.

        If exact is non-empty it takes precedence over prefix.
        """
        with psycopg.connect(self._connstr, row_factory=dict_row, options="-c search_path=entroq,public") as conn:
            return conn.execute(
                'SELECT * FROM queues(%s, %s, %s)',
                (prefix, list(exact), limit),
            ).fetchall()

    def tasks(self, queue: str = '', limit: int = 0, omit_values: bool = False) -> List[Task]:
        """Return tasks ordered by at. queue='' means all queues."""
        with psycopg.connect(self._connstr, row_factory=dict_row, options="-c search_path=entroq,public") as conn:
            return [_row_to_task(r) for r in conn.execute(
                'SELECT * FROM tasks(%s, %s, %s)',
                (queue, limit, omit_values),
            ).fetchall()]

    def try_claim(self, queue: Union[str, List[str]], duration_ms: int = 30000) -> Optional[Task]:
        """Try to claim one task from queue (str or list of str).

        Returns None if no task is available in any of the given queues.
        """
        queues = [queue] if isinstance(queue, str) else list(queue)
        with psycopg.connect(self._connstr, row_factory=dict_row, options="-c search_path=entroq,public") as conn:
            rows = conn.execute(
                "SELECT * FROM try_claim(%s, %s, %s::interval)",
                (queues, self._claimant, f'{duration_ms} milliseconds'),
            ).fetchall()
        return _row_to_task(rows[0]) if rows else None

    def claim(self, queue: Union[str, List[str]], duration_ms: int = 30000, poll_ms: int = 5000, timeout_s: Optional[float] = None, poll_only: bool = False) -> Task:
        """Block until a task is available in queue (str or list), then claim it.

        By default uses PostgreSQL LISTEN/NOTIFY to wake up promptly when a
        task arrives, falling back to poll_ms-interval polling if no
        notification arrives.
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

        queues = [queue] if isinstance(queue, str) else list(queue)
        channels = [_pg_channel_name(q) for q in queues]
        with psycopg.connect(self._connstr, options="-c search_path=entroq,public") as lconn:
            lconn.autocommit = True
            for ch in channels:
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
        """Context manager that runs EntroQ operations and user SQL in one transaction."""
        with psycopg.connect(self._connstr, row_factory=dict_row, options="-c search_path=entroq,public") as conn:
            yield Transaction(conn, self._claimant)

    def modify(
        self,
        inserts: List[TaskData] = (),
        changes: List[TaskChange] = (),
        deletes: List[Union[Task, TaskID]] = (),
        depends: List[Union[Task, TaskID]] = (),
        unsafe_claimant_id: Optional[str] = None,
    ) -> Tuple[List[Task], List[Task]]:
        """Atomically apply inserts, changes, deletes, and dependency checks."""
        with self.transaction() as txn:
            return txn.modify(
                inserts=inserts,
                changes=changes,
                deletes=deletes,
                depends=depends,
                unsafe_claimant_id=unsafe_claimant_id,
            )

    def delete(self, task: Union[Task, TaskID]):
        """Delete a single task (Task or TaskID)."""
        self.modify(deletes=[task])

    def renew_for(self, task: Task, duration: int = 30) -> Task:
        """Extend a task's claim expiry by duration seconds."""
        at = self.time() + timedelta(seconds=duration)
        _, changed = self.modify(changes=[task.as_change(at=at)])
        return changed[0]

    @contextmanager
    def renewing(self, task: Task, duration: int = 30):
        """Context manager that renews a task claim in the background."""
        with common_renewing(self, task, duration_s=duration) as current_task:
            yield current_task

    def pop_all(self, queue: str, force: bool = False) -> Iterator[Task]:
        """Claim and delete every task in queue, yielding each one."""
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



class EQWorker(common_worker):
    """Claims tasks from a queue and dispatches them to a handler function using EntroQ client."""
    pass
