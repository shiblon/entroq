"""Integration tests for entroq_pg (async).

All tests require a running Docker daemon. The pg_connstr session fixture in
conftest.py starts a throwaway PostgreSQL container and applies schema.sql
once per test session. The eq function fixture truncates the tasks table
before each test.

These tests focus on Python+native-PostgreSQL-specific behavior: LISTEN/NOTIFY
wakeup, the renewing context manager, and transaction composability with
user SQL.  Protocol-level correctness (insert/claim/delete semantics) is covered
by the Go qtest suite run against the same stored procedures.
"""

import asyncio
import time

import psycopg
import pytest

from entroq.pg import EntroQ
from entroq.types import DependencyError, Modification, TaskData, TaskID
from entroq.worker import EntroQWorker, Handler, _renewing


# ---------------------------------------------------------------------------
# Sanity
# ---------------------------------------------------------------------------

async def test_db_time(eq: EntroQ):
    """Database is reachable and returns a timestamp."""
    assert await eq.time() is not None


# ---------------------------------------------------------------------------
# LISTEN/NOTIFY
# ---------------------------------------------------------------------------

async def test_claim_unblocks_on_notify(eq: EntroQ):
    """claim() should wake promptly on NOTIFY rather than waiting the full poll interval."""
    QUEUE = '/test/notify'
    POLL_MS = 10_000

    claimed = []

    async def _claimer():
        claimed.append(await eq.claim(QUEUE, poll_ms=POLL_MS))

    claim_task = asyncio.create_task(_claimer())
    await asyncio.sleep(0.3)  # let claim() reach lconn.notifies()

    insert_time = time.monotonic()
    await eq.modify(Modification(Modification.inserting(TaskData(queue=QUEUE, value='ping'))))

    try:
        await asyncio.wait_for(claim_task, timeout=5.0)
    except asyncio.TimeoutError:
        pytest.fail('claim() did not unblock within 5s after insert')

    elapsed = time.monotonic() - insert_time
    assert len(claimed) == 1
    assert elapsed < 3.0, f'claim() took {elapsed:.2f}s -- NOTIFY may not have fired'


# ---------------------------------------------------------------------------
# renewing -- renewal correctness
# ---------------------------------------------------------------------------

async def test_renewal_updates_task_version(eq: EntroQ):
    """Renewer task should bump the task version during the renewing block."""
    QUEUE = '/test/renew'
    DURATION = 1.0

    await eq.modify(Modification(Modification.inserting(TaskData(queue=QUEUE, value='work'))))
    task = await eq.try_claim(QUEUE, duration_ms=int(DURATION * 1000))
    assert task is not None
    initial_version = task.version

    async with _renewing(eq, task, [], DURATION) as state:
        await asyncio.sleep(DURATION * 1.5)
        version_during = state.task.version

    assert version_during > initial_version, (
        f'version should have increased after renewal '
        f'(initial={initial_version}, during={version_during})'
    )
    assert state.task.version >= version_during


# ---------------------------------------------------------------------------
# renewing -- concurrency
# ---------------------------------------------------------------------------

async def test_concurrent_renewing_no_deadlock(eq: EntroQ):
    """Many concurrent renewing blocks should all complete without deadlocking."""
    N = 20
    QUEUE = '/test/concurrent_renew'
    DURATION = 1.0

    await eq.modify(Modification(*[
        Modification.inserting(TaskData(queue=QUEUE, value=str(i))) for i in range(N)
    ]))

    errors = []
    results = []

    async def run_one():
        task = await eq.try_claim(QUEUE, duration_ms=int(DURATION * 1000 * 10))
        if task is None:
            return
        try:
            async with _renewing(eq, task, [], DURATION) as state:
                await asyncio.sleep(DURATION * 1.5)
                version_during = state.task.version
            results.append((state.task.id, version_during))
        except Exception as exc:
            errors.append(exc)

    await asyncio.gather(*[asyncio.create_task(run_one()) for _ in range(N)])

    assert not errors, f'exceptions in tasks: {errors}'
    assert len(results) == N, f'expected {N} results, got {len(results)}'

    for task_id, mid_version in results:
        assert mid_version > 1, (
            f'task {task_id}: mid-handler version {mid_version} was not renewed'
        )


# ---------------------------------------------------------------------------
# Transaction composability
# ---------------------------------------------------------------------------

async def test_transaction_commits_atomically(eq: EntroQ, pg_connstr: str):
    """txn.modify() and user SQL in the same transaction should commit together."""
    QUEUE = '/test/txn'
    await eq.modify(Modification(Modification.inserting(TaskData(queue=QUEUE, value='item'))))
    task = await eq.try_claim(QUEUE)
    assert task is not None

    async with eq.transaction() as txn:
        await txn.conn.execute(
            "INSERT INTO test_counter (name, count) VALUES ('done', 1)"
            " ON CONFLICT (name) DO UPDATE SET count = test_counter.count + 1"
        )
        await txn.modify(Modification(Modification.deleting(task)))

    with psycopg.connect(pg_connstr) as conn:
        row = conn.execute("SELECT count FROM test_counter WHERE name = 'done'").fetchone()
    assert row[0] == 1
    assert await eq.tasks(queue=QUEUE) == []


async def test_transaction_rollback_on_dependency_error(eq: EntroQ, pg_connstr: str):
    """A DependencyError inside a transaction should roll back user SQL too."""
    QUEUE = '/test/txn_rollback'
    await eq.modify(Modification(Modification.inserting(TaskData(queue=QUEUE, value='item'))))
    task = await eq.try_claim(QUEUE)
    assert task is not None

    with pytest.raises(DependencyError):
        async with eq.transaction() as txn:
            await txn.conn.execute(
                "INSERT INTO test_counter (name, count) VALUES ('done', 1)"
                " ON CONFLICT (name) DO UPDATE SET count = test_counter.count + 1"
            )
            # Wrong version — forces a DependencyError and should roll back everything.
            await txn.modify(Modification(Modification.deleting(TaskID(id=task.id, version=task.version + 99))))

    with psycopg.connect(pg_connstr) as conn:
        row = conn.execute("SELECT count FROM test_counter WHERE name = 'done'").fetchone()
    assert row is None or row[0] == 0


# ---------------------------------------------------------------------------
# Stress: concurrent workers with renewal and transactional counter
# ---------------------------------------------------------------------------

async def test_concurrent_workers_transactional_counter(eq: EntroQ, pg_connstr: str):
    """Many concurrent workers claiming, renewing, and committing atomically.

    N tasks are inserted. W workers race to claim and process them, each
    deleting the task and incrementing a shared counter in a single transaction
    via renewing + transaction. The final counter must equal exactly N -- any
    double-claim, lost transaction, or renewal bug would produce the wrong value.
    """
    N = 50
    W = 10
    QUEUE = '/test/stress'
    DURATION = 2.0

    await eq.modify(Modification(*[
        Modification.inserting(TaskData(queue=QUEUE, value=str(i))) for i in range(N)
    ]))

    errors = []

    async def worker():
        while True:
            task = await eq.try_claim(QUEUE, duration_ms=int(DURATION * 1000 * 5))
            if task is None:
                return
            try:
                async with _renewing(eq, task, [], DURATION) as state:
                    await asyncio.sleep(DURATION * 1.2)
                async with eq.transaction() as txn:
                    await txn.conn.execute(
                        "INSERT INTO test_counter (name, count) VALUES ('done', 1)"
                        " ON CONFLICT (name) DO UPDATE SET count = test_counter.count + 1"
                    )
                    await txn.modify(Modification(Modification.deleting(state.task)))
            except Exception as exc:
                errors.append(exc)
                return

    await asyncio.gather(*[asyncio.create_task(worker()) for _ in range(W)])

    assert not errors, f'exceptions in worker tasks: {errors}'

    with psycopg.connect(pg_connstr) as conn:
        row = conn.execute("SELECT count FROM test_counter WHERE name = 'done'").fetchone()
    final_count = row[0] if row else 0
    assert final_count == N, f'expected counter={N}, got {final_count} -- possible double-claim or lost transaction'
    assert await eq.tasks(queue=QUEUE) == [], 'tasks remain in queue after all workers finished'


async def test_concurrent_workers_multi_queue(eq: EntroQ, pg_connstr: str):
    """Workers claiming from multiple queues simultaneously, with renewal and transactional counter."""
    N = 50
    W = 10
    Q = 5
    DURATION = 2.0
    QUEUES = [f'/test/multi/{i}' for i in range(Q)]

    await eq.modify(Modification(*[
        Modification.inserting(TaskData(queue=QUEUES[i % Q], value=str(i)))
        for i in range(N)
    ]))

    errors = []

    async def worker():
        while True:
            task = await eq.try_claim(QUEUES, duration_ms=int(DURATION * 1000 * 5))
            if task is None:
                return
            try:
                async with _renewing(eq, task, [], DURATION) as state:
                    await asyncio.sleep(DURATION * 1.2)
                async with eq.transaction() as txn:
                    await txn.conn.execute(
                        "INSERT INTO test_counter (name, count) VALUES ('done', 1)"
                        " ON CONFLICT (name) DO UPDATE SET count = test_counter.count + 1"
                    )
                    await txn.modify(Modification(Modification.deleting(state.task)))
            except Exception as exc:
                errors.append(exc)
                return

    await asyncio.gather(*[asyncio.create_task(worker()) for _ in range(W)])

    assert not errors, f'exceptions in worker tasks: {errors}'

    with psycopg.connect(pg_connstr) as conn:
        row = conn.execute("SELECT count FROM test_counter WHERE name = 'done'").fetchone()
    final_count = row[0] if row else 0
    assert final_count == N, f'expected counter={N}, got {final_count}'
    for q in QUEUES:
        assert await eq.tasks(queue=q) == [], f'tasks remain in queue {q}'


async def test_multi_queue_fairness(eq: EntroQ):
    """Multi-queue try_claim should drain smaller queues proportionally early."""
    BIG_SIZE = 300
    MED_SIZE = 60
    SMALL_SIZE = 20
    N_WORKERS = 5

    big_q   = '/test/fairness/big'
    med_q   = '/test/fairness/med'
    small_q = '/test/fairness/small'
    queues  = [big_q, med_q, small_q]

    await eq.modify(Modification(*[
        Modification.inserting(TaskData(queue=q, value='x'))
        for q, n in ((big_q, BIG_SIZE), (med_q, MED_SIZE), (small_q, SMALL_SIZE))
        for _ in range(n)
    ]))

    consumed = []
    errors = []

    async def worker():
        while True:
            try:
                task = await eq.claim(queues, timeout_s=3)
            except TimeoutError:
                task_lists = [await eq.tasks(queue=q) for q in queues]
                remaining = sum(1 for tl in task_lists if tl)
                if remaining:
                    errors.append(RuntimeError(
                        f'claim timed out with {remaining} tasks still in queues '
                        f'(consumed so far: {len(consumed)})'
                    ))
                return
            consumed.append(task.queue)
            try:
                await eq.modify(Modification(Modification.deleting(task)))
            except Exception as exc:
                errors.append(exc)
                return

    await asyncio.gather(*[asyncio.create_task(worker()) for _ in range(N_WORKERS)])

    assert not errors, f'worker errors: {errors}'
    total = len(consumed)
    assert total == BIG_SIZE + MED_SIZE + SMALL_SIZE, (
        f'expected {BIG_SIZE + MED_SIZE + SMALL_SIZE} tasks consumed, got {total}'
    )

    last_small = max((i for i, q in enumerate(consumed) if q == small_q), default=-1)
    last_med   = max((i for i, q in enumerate(consumed) if q == med_q),   default=-1)

    assert last_small < total // 3, (
        f'small queue not exhausted fairly: last task at position {last_small}/{total} '
        f'(threshold {total // 3})'
    )
    assert last_med < total * 3 // 4, (
        f'med queue not exhausted fairly: last task at position {last_med}/{total} '
        f'(threshold {total * 3 // 4})'
    )


# ---------------------------------------------------------------------------
# EntroQWorker integration
# ---------------------------------------------------------------------------

async def test_worker_processes_tasks(eq: EntroQ):
    """Worker should claim and complete N tasks."""
    N = 5
    QUEUE = '/test/worker/basic'

    await eq.modify(Modification(*[
        Modification.inserting(TaskData(queue=QUEUE, value=str(i))) for i in range(N)
    ]))

    completed = []
    worker = EntroQWorker(eq, QUEUE)

    @EntroQWorker.handler
    async def handle(task, docs):
        completed.append(task.value)
        if len(completed) >= N:
            worker.stop()
        return Modification(Modification.deleting(task))

    await worker.run(handle)

    assert len(completed) == N
    assert await eq.tasks(queue=QUEUE) == []


async def test_worker_stable_version_in_finalize(eq: EntroQ):
    """Version seen in finish() after renewal should be >= version at claim time."""
    QUEUE = '/test/worker/version'
    DURATION = 1.0

    await eq.modify(Modification(Modification.inserting(TaskData(queue=QUEUE, value='work'))))

    versions = {}
    worker_obj = EntroQWorker(eq, QUEUE, claim_duration_s=DURATION)

    class VersionHandler(Handler):
        async def do_work(self, task, docs):
            versions['claimed'] = task.version
            await asyncio.sleep(DURATION * 1.5)  # trigger at least one renewal
            return None

        async def finish(self, task, docs):
            versions['finish'] = task.version
            worker_obj.stop()
            await eq.modify(Modification(Modification.deleting(task)))

    await worker_obj.run(VersionHandler())

    assert 'claimed' in versions and 'finish' in versions
    assert versions['finish'] >= versions['claimed'], (
        f"finish version {versions['finish']} should be >= claimed version {versions['claimed']}"
    )
    assert await eq.tasks(queue=QUEUE) == []


async def test_worker_continues_after_dependency_error(eq: EntroQ):
    """Worker should log and continue when do_work raises a DependencyError."""
    QUEUE = '/test/worker/dep_err'

    await eq.modify(Modification(Modification.inserting(TaskData(queue=QUEUE, value='work'))))

    outcomes = []
    calls = [0]
    worker_obj = EntroQWorker(eq, QUEUE, claim_duration_s=0.5)

    @EntroQWorker.handler
    async def handle(task, docs):
        calls[0] += 1
        if calls[0] == 1:
            raise DependencyError('simulated')
        outcomes.append(task.value)
        worker_obj.stop()
        return Modification(Modification.deleting(task))

    await worker_obj.run(handle)

    assert calls[0] == 2, f'expected 2 calls (1 dep error + 1 success), got {calls[0]}'
    assert outcomes == ['work']
    assert await eq.tasks(queue=QUEUE) == []
