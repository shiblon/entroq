"""EntroQ PostgreSQL client — async.

Talks directly to a PostgreSQL database initialized by the Go eqpg backend.
Requires psycopg >= 3 (psycopg3).
"""
from __future__ import annotations

import hashlib
import json
import re
import time
import uuid
from contextlib import asynccontextmanager
from datetime import datetime
from typing import AsyncIterator

import psycopg
from psycopg.rows import dict_row

from ...types import Task, Doc, DependencyError, Modification, ModifyResult, _DOC_RELEASE_AT
from ...base import EntroQBase


# ---------------------------------------------------------------------------
# LISTEN/NOTIFY channel name — must mirror pgChannelName() in eqpg/pgnotify.go
# ---------------------------------------------------------------------------

_nonalnum = re.compile(r'[^a-zA-Z0-9]')

def _pg_channel_name(queue: str) -> str:
    sanitized = _nonalnum.sub('_', queue)
    if len(sanitized) + 2 <= 63:
        return 'q_' + sanitized
    h = hashlib.md5(queue.encode()).hexdigest()
    return 'q_' + sanitized[:25] + '_' + h[:8] + '_' + sanitized[-26:]


def _pg_interval(duration_ms: int) -> str:
    """Format a millisecond duration as a Postgres interval literal.

    Split into whole seconds plus a milliseconds remainder so that no single
    interval field integer exceeds int32, which Postgres rejects (SQLSTATE
    22015, "interval field value out of range"). A bare milliseconds field
    overflows at ~24.8 days (INT32_MAX milliseconds); expressing whole seconds
    separately pushes that ceiling out to ~68 years.
    """
    secs, ms = divmod(duration_ms, 1000)
    return f'{secs} seconds {ms} milliseconds'


# ---------------------------------------------------------------------------
# Row → dataclass helpers
# ---------------------------------------------------------------------------

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

def _row_to_doc(row: dict) -> Doc:
    return Doc(
        namespace=row['namespace'],
        id=str(row['id']),
        version=row['version'],
        key=row['key_primary'],
        secondary_key=row['key_secondary'],
        content=row['value'],
        claimant=str(row['claimant']) if row['claimant'] else '',
        at=row['at'],
        created=row['created'],
        modified=row['modified'],
    )


# ---------------------------------------------------------------------------
# JSONB encoding helpers for stored procedures
# ---------------------------------------------------------------------------

def _encode_task_ids(items) -> str:
    # queue is part of the modify key: the backend matches it against stored state.
    return json.dumps([{'id': str(i.id), 'version': i.version, 'queue': i.queue} for i in items])

def _encode_task_inserts(items) -> str:
    out = []
    for it in items:
        obj: dict = {'queue': it.queue, 'value': it.value}
        if it.id:       obj['id'] = str(it.id)
        if it.at:       obj['at'] = it.at.isoformat()
        if it.attempt:  obj['attempt'] = it.attempt
        if it.err:      obj['err'] = it.err
        out.append(obj)
    return json.dumps(out)

def _encode_task_changes(items) -> str:
    out = []
    for c in items:
        obj: dict = {
            'id': str(c.id), 'version': c.version,
            # from_queue (source) is part of the modify key; queue is the destination.
            'from_queue': c.from_queue, 'queue': c.queue, 'value': c.value,
        }
        if c.at:        obj['at'] = c.at.isoformat()
        if c.attempt:   obj['attempt'] = c.attempt
        if c.err:       obj['err'] = c.err
        out.append(obj)
    return json.dumps(out)

def _encode_doc_ids(items) -> str:
    return json.dumps([{'namespace': i.namespace, 'id': str(i.id), 'version': i.version} for i in items])

def _encode_doc_inserts(items) -> str:
    out = []
    for it in items:
        obj: dict = {
            'namespace': it.namespace,
            'key_primary': it.key,
            'key_secondary': it.secondary_key,
        }
        if it.id:       obj['id'] = str(it.id)
        if it.content is not None: obj['content'] = it.content
        out.append(obj)
    return json.dumps(out)

def _encode_doc_changes(items) -> str:
    out = []
    for c in items:
        obj: dict = {
            'namespace': c.namespace, 'id': str(c.id), 'version': c.version,
            'key_primary': c.key, 'key_secondary': c.secondary_key,
            'at': (c.at or _DOC_RELEASE_AT).isoformat(),
        }
        if c.content is not None: obj['content'] = c.content
        out.append(obj)
    return json.dumps(out)


# ---------------------------------------------------------------------------
# DependencyError parsing from psycopg DatabaseError
# ---------------------------------------------------------------------------

def _dep_error_from_pg(e: psycopg.DatabaseError) -> DependencyError:
    detail = json.loads(e.diag.message_detail)
    return DependencyError(
        message=str(e),
        missing=detail.get('missing', []),
        mismatched=detail.get('mismatched', []),
        collisions=detail.get('collisions', []),
    )


# ---------------------------------------------------------------------------
# Transaction (pg-specific: combines task + doc ops in one connection tx)
# ---------------------------------------------------------------------------

_OPTS = "-c search_path=entroq,public"


class Transaction:
    """EntroQ operations sharing a single database transaction.

    Obtain via ``async with client.transaction() as txn:``.
    The ``conn`` attribute is the underlying psycopg AsyncConnection and may
    be used for arbitrary SQL within the same transaction.
    """

    def __init__(self, conn: psycopg.AsyncConnection, claimant: str) -> None:
        self.conn = conn
        self._claimant = claimant

    async def modify(self, modification: Modification, *, unsafe_claimant_id: str | None = None) -> ModifyResult:
        """Atomically apply task and doc operations within the enclosing transaction."""
        claimant = unsafe_claimant_id or self._claimant
        task_inserted: list[Task] = []
        task_changed: list[Task] = []
        doc_inserted: list[Doc] = []
        doc_changed: list[Doc] = []

        # Task operations
        has_task_ops = any([
            modification.task_inserts, modification.task_changes,
            modification.task_deletes, modification.task_depends,
        ])
        if has_task_ops:
            try:
                cur = await self.conn.execute(
                    '''SELECT kind, id, version, queue, at, created, modified,
                              claimant, value, claims, attempt, err
                       FROM modify(%s, %s::jsonb, %s::jsonb, %s::jsonb, %s::jsonb)''',
                    (
                        claimant,
                        _encode_task_ids(modification.task_depends),
                        _encode_task_ids(modification.task_deletes),
                        _encode_task_inserts(modification.task_inserts),
                        _encode_task_changes(modification.task_changes),
                    ),
                )
                for row in await cur.fetchall():
                    t = _row_to_task(row)
                    (task_inserted if row['kind'] == 'inserted' else task_changed).append(t)
            except psycopg.DatabaseError as e:
                if e.diag.sqlstate == 'EQ001':
                    raise _dep_error_from_pg(e) from e
                raise

        # Doc operations
        has_doc_ops = any([
            modification.doc_inserts, modification.doc_changes,
            modification.doc_deletes, modification.doc_depends,
        ])
        if has_doc_ops:
            try:
                cur = await self.conn.execute(
                    '''SELECT kind, namespace, id, version, claimant, at,
                              key_primary, key_secondary, value, created, modified
                       FROM modify_docs(%s, %s::jsonb, %s::jsonb, %s::jsonb, %s::jsonb)''',
                    (
                        claimant,
                        _encode_doc_ids(modification.doc_depends),
                        _encode_doc_ids(modification.doc_deletes),
                        _encode_doc_inserts(modification.doc_inserts),
                        _encode_doc_changes(modification.doc_changes),
                    ),
                )
                for row in await cur.fetchall():
                    d = _row_to_doc(row)
                    (doc_inserted if row['kind'] == 'inserted' else doc_changed).append(d)
            except psycopg.DatabaseError as e:
                if e.diag.sqlstate == 'EQ001':
                    raise _dep_error_from_pg(e) from e
                raise

        return ModifyResult(task_inserted, task_changed, doc_inserted, doc_changed)


# ---------------------------------------------------------------------------
# Schema version check (sync: runs once at startup)
# ---------------------------------------------------------------------------

SCHEMA_VERSION = "1.7.1"

_INIT_HINT = (
    "Initialize the database with:\n\n"
    "  docker run --rm shiblon/entroq:1 schema init --db YOUR_DSN\n\n"
    "where YOUR_DSN is a libpq connection string, e.g.:\n"
    "  'host=localhost dbname=entroq user=entroq password=secret'"
)

def _check_schema_version(connstr: str) -> None:
    with psycopg.connect(connstr, row_factory=dict_row, options=_OPTS) as conn:
        try:
            row = conn.execute("SELECT value FROM meta WHERE key = 'schema_version'").fetchone()
        except psycopg.DatabaseError as e:
            if e.diag.sqlstate == '42P01':
                raise RuntimeError(f"EntroQ schema not found.\n{_INIT_HINT}") from e
            raise
        if row is None:
            raise RuntimeError(f"EntroQ schema not initialized (schema_version missing).\n{_INIT_HINT}")
        stored = row['value']
        if stored != SCHEMA_VERSION:
            raise RuntimeError(
                f"EntroQ schema version mismatch: database has {stored!r}, "
                f"client expects {SCHEMA_VERSION!r}.\n{_INIT_HINT}"
            )


# ---------------------------------------------------------------------------
# Main async client
# ---------------------------------------------------------------------------

class EntroQ(EntroQBase):
    """EntroQ client that connects directly to PostgreSQL (async)."""

    def __init__(self, connstr: str) -> None:
        self._connstr = connstr
        self._claimant = str(uuid.uuid4())
        _check_schema_version(connstr)

    async def time(self) -> datetime:
        async with await psycopg.AsyncConnection.connect(self._connstr, autocommit=True, row_factory=dict_row, options=_OPTS) as conn:
            row = await (await conn.execute('SELECT now() AS t')).fetchone()
            return row['t']

    async def queues(self, prefix: str = '', exact=(), limit: int = 0) -> list[dict]:
        async with await psycopg.AsyncConnection.connect(self._connstr, autocommit=True, row_factory=dict_row, options=_OPTS) as conn:
            cur = await conn.execute('SELECT * FROM queues(%s, %s, %s)', (prefix, list(exact), limit))
            return await cur.fetchall()

    async def tasks(self, queue: str = '', limit: int = 0, omit_values: bool = False) -> list[Task]:
        async with await psycopg.AsyncConnection.connect(self._connstr, autocommit=True, row_factory=dict_row, options=_OPTS) as conn:
            cur = await conn.execute('SELECT * FROM tasks(%s, %s, %s)', (queue, limit, omit_values))
            return [_row_to_task(r) for r in await cur.fetchall()]

    async def try_claim(self, queue: str | list[str], duration_ms: int = 30000) -> Task | None:
        queues = [queue] if isinstance(queue, str) else list(queue)
        async with await psycopg.AsyncConnection.connect(self._connstr, autocommit=True, row_factory=dict_row, options=_OPTS) as conn:
            cur = await conn.execute(
                "SELECT * FROM try_claim(%s, %s, %s::interval)",
                (queues, self._claimant, _pg_interval(duration_ms)),
            )
            rows = await cur.fetchall()
        return _row_to_task(rows[0]) if rows else None

    async def claim(self, queue: str | list[str], duration_ms: int = 30000, poll_ms: int = 5000, timeout_s: float | None = None) -> Task:
        queues = [queue] if isinstance(queue, str) else list(queue)
        channels = [_pg_channel_name(q) for q in queues]
        deadline = None if timeout_s is None else time.monotonic() + timeout_s

        async with await psycopg.AsyncConnection.connect(self._connstr, autocommit=True, options=_OPTS) as lconn:
            for ch in channels:
                await lconn.execute(f'LISTEN "{ch}"')

            while True:
                task = await self.try_claim(queue, duration_ms)
                if task is not None:
                    return task

                if deadline is not None and time.monotonic() >= deadline:
                    raise TimeoutError(f'claim timed out after {timeout_s}s')

                wait = poll_ms / 1000
                if deadline is not None:
                    wait = min(wait, deadline - time.monotonic())

                async for _ in lconn.notifies(timeout=wait):
                    break  # one notification is enough; retry the claim

    @asynccontextmanager
    async def transaction(self) -> AsyncIterator[Transaction]:
        """Async context manager: runs EntroQ operations and user SQL in one transaction."""
        async with await psycopg.AsyncConnection.connect(self._connstr, row_factory=dict_row, options=_OPTS) as conn:
            yield Transaction(conn, self._claimant)

    async def modify(self, modification: Modification, *, unsafe_claimant_id: str | None = None) -> ModifyResult:
        async with self.transaction() as txn:
            return await txn.modify(modification, unsafe_claimant_id=unsafe_claimant_id)

    async def docs(
        self,
        namespace: str = '',
        key_start: str = '',
        key_end: str = '',
        limit: int = 0,
        omit_values: bool = False,
    ) -> list[Doc]:
        async with await psycopg.AsyncConnection.connect(self._connstr, autocommit=True, row_factory=dict_row, options=_OPTS) as conn:
            cur = await conn.execute(
                'SELECT * FROM docs(%s, %s, %s, %s, %s)',
                (namespace, key_start, key_end, limit, omit_values),
            )
            return [_row_to_doc(r) for r in await cur.fetchall()]

    async def claim_docs(self, namespace: str, key: str, duration_ms: int = 30000) -> list[Doc]:
        async with await psycopg.AsyncConnection.connect(self._connstr, autocommit=True, row_factory=dict_row, options=_OPTS) as conn:
            try:
                cur = await conn.execute(
                    "SELECT * FROM claim_docs(%s, %s, %s::interval, %s)",
                    (namespace, self._claimant, _pg_interval(duration_ms), key),
                )
                return [_row_to_doc(r) for r in await cur.fetchall()]
            except psycopg.DatabaseError as e:
                if e.diag.sqlstate == 'EQ001':
                    detail = json.loads(e.diag.message_detail)
                    raise DependencyError(
                        message=str(e),
                        missing=detail.get('missing_docs', []),
                        collisions=detail.get('claimed_docs', []),
                    ) from e
                raise

    async def gc_queues(self) -> list[tuple[str, datetime | None]]:
        """Discover queues that opt into garbage collection, with activation times.

        Returns one ``(queue, activate_at)`` pair per queue whose name carries a
        ``/gc=`` component. A ``None`` activate_at marks a malformed gc= value:
        the queue opted into collection but its timestamp will not parse, so it
        is never collected and should be surfaced as a misconfiguration. The gc=
        grammar lives entirely in the SQL (``gc_activation``); this client never
        parses it. Feed these rows straight to :meth:`gc_collect`.
        """
        async with await psycopg.AsyncConnection.connect(self._connstr, autocommit=True, row_factory=dict_row, options=_OPTS) as conn:
            rows = await (await conn.execute('SELECT queue, activate_at FROM gc_queues()')).fetchall()
            return [(r['queue'], r['activate_at']) for r in rows]

    async def gc_collect(self, queues: list[str], activations: list[datetime | None], batch: int = 1000) -> int:
        """Delete up to ``batch`` due, collectable tasks from the given queues.

        Pass the ``(queue, activate_at)`` pairs from :meth:`gc_queues` verbatim,
        malformed ones included: ``gc_collect`` only collects where
        ``activate_at <= now()`` on the database clock, which discards both
        malformed queues (NULL activation) and not-yet-due ones (future
        activation). This client does no parsing or clock arithmetic. Returns the
        number deleted; drain by calling until the result is ``< batch``.

        Exposed so a direct-PostgreSQL worker can reap on the backend's behalf
        (a Go server GCs itself; a worker talking to it must not).
        """
        async with await psycopg.AsyncConnection.connect(self._connstr, autocommit=True, row_factory=dict_row, options=_OPTS) as conn:
            rows = await (await conn.execute(
                'SELECT deleted FROM gc_collect(%s::text[], %s::timestamptz[], %s)',
                (queues, activations, batch),
            )).fetchall()
            return sum(r['deleted'] for r in rows)

    async def pop_all(self, queue: str, force: bool = False) -> AsyncIterator[Task]:
        """Claim and delete every task in queue, yielding each."""
        if force:
            for task in await self.tasks(queue=queue):
                await self.modify(
                    Modification(Modification.deleting(task.as_id())),
                    unsafe_claimant_id=task.claimant,
                )
                yield task
            return
        while True:
            task = await self.try_claim(queue)
            if task is None:
                return
            await self.modify(Modification(Modification.deleting(task.as_id())))
            yield task
