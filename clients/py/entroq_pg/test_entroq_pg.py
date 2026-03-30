"""Integration tests for entroq_pg.

All tests require a running Docker daemon. The pg_connstr session fixture in
conftest.py starts a throwaway PostgreSQL container and applies schema.sql
once per test session. The eq function fixture truncates the tasks table
before each test.

These tests focus on Python-specific behavior: LISTEN/NOTIFY wakeup, the
threading inside do_with_renew, and transaction composability with user SQL.
Protocol-level correctness (insert/claim/delete semantics) is covered by the
Go qtest suite run against the same stored procedures.
"""

import threading
import time

import psycopg
import pytest

from entroq_pg import DependencyError, EntroQ, TaskData, TaskID


# ---------------------------------------------------------------------------
# Sanity
# ---------------------------------------------------------------------------

def test_db_time(eq: EntroQ):
    """Database is reachable and returns a timestamp."""
    assert eq.time() is not None


# ---------------------------------------------------------------------------
# LISTEN/NOTIFY
# ---------------------------------------------------------------------------

def test_claim_unblocks_on_notify(eq: EntroQ):
    """claim() should wake promptly on NOTIFY rather than waiting the full poll interval."""
    QUEUE = '/test/notify'
    # Long poll interval so the test fails obviously if NOTIFY doesn't fire.
    POLL_MS = 10_000

    claimed = []
    ready = threading.Event()

    def _claimer():
        # Signal just before blocking so the main thread knows when to insert.
        # We can't hook into claim() itself, so we approximate with a short
        # pre-sleep that lets the LISTEN connection get established.
        ready.set()
        claimed.append(eq.claim(QUEUE, poll_ms=POLL_MS))

    t = threading.Thread(target=_claimer, daemon=True)
    t.start()
    ready.wait()
    time.sleep(0.3)  # let claim() reach lconn.notifies()

    insert_time = time.monotonic()
    eq.modify(inserts=[TaskData(queue=QUEUE, value=b'ping')])
    t.join(timeout=5.0)
    elapsed = time.monotonic() - insert_time

    assert not t.is_alive(), 'claim() did not unblock within 5s after insert'
    assert len(claimed) == 1
    # With NOTIFY the wakeup should be nearly immediate; 3s is very generous.
    assert elapsed < 3.0, f'claim() took {elapsed:.2f}s -- NOTIFY may not have fired'


# ---------------------------------------------------------------------------
# do_with_renew -- renewal correctness
# ---------------------------------------------------------------------------

def test_renewal_updates_task_version(eq: EntroQ):
    """Renewer thread should bump the task version; task() should return the new version."""
    QUEUE = '/test/renew'
    DURATION = 1  # seconds; renewer fires at 0.5s

    eq.modify(inserts=[TaskData(queue=QUEUE, value=b'work')])
    task = eq.try_claim(QUEUE, duration_ms=DURATION * 1000)
    assert task is not None
    initial_version = task.version

    version_mid_handler = []

    def handler(task_fn):
        time.sleep(DURATION * 1.5)  # sleep past first renewal window
        version_mid_handler.append(task_fn().version)

    final_task, _ = eq.do_with_renew(task, handler, duration=DURATION)

    assert version_mid_handler[0] > initial_version, (
        f'version should have increased after renewal '
        f'(initial={initial_version}, mid={version_mid_handler[0]})'
    )
    assert final_task.version >= version_mid_handler[0]


# ---------------------------------------------------------------------------
# do_with_renew -- thread safety
# ---------------------------------------------------------------------------

def test_concurrent_do_with_renew_no_deadlock(eq: EntroQ):
    """Many concurrent do_with_renew calls should all complete without deadlocking."""
    N = 20
    QUEUE = '/test/concurrent_renew'
    DURATION = 1  # renewer fires at 0.5s

    eq.modify(inserts=[TaskData(queue=QUEUE, value=str(i).encode()) for i in range(N)])

    errors = []
    results = []
    lock = threading.Lock()

    def run_one():
        task = eq.try_claim(QUEUE, duration_ms=DURATION * 1000 * 10)
        if task is None:
            return
        def handler(task_fn):
            time.sleep(DURATION * 1.5)  # ensure at least one renewal
            return task_fn().version
        try:
            final, version = eq.do_with_renew(task, handler, duration=DURATION)
            with lock:
                results.append((final.id, version))
        except Exception as exc:
            with lock:
                errors.append(exc)

    threads = [threading.Thread(target=run_one) for _ in range(N)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=30)

    still_alive = sum(1 for t in threads if t.is_alive())
    assert still_alive == 0, f'{still_alive} threads still running -- possible deadlock'
    assert not errors, f'exceptions in worker threads: {errors}'
    assert len(results) == N, f'expected {N} results, got {len(results)}'

    # Every renewal should have bumped the version at least once.
    initial_claimed_version = 1  # version after a single claim
    for task_id, mid_version in results:
        assert mid_version > initial_claimed_version, (
            f'task {task_id}: mid-handler version {mid_version} was not renewed'
        )


# ---------------------------------------------------------------------------
# Transaction composability
# ---------------------------------------------------------------------------

def test_transaction_commits_atomically(eq: EntroQ, pg_connstr: str):
    """txn.modify() and user SQL in the same transaction should commit together."""
    QUEUE = '/test/txn'
    eq.modify(inserts=[TaskData(queue=QUEUE, value=b'item')])
    task = eq.try_claim(QUEUE)
    assert task is not None

    with eq.transaction() as txn:
        txn.conn.execute(
            "INSERT INTO test_counter (name, count) VALUES ('done', 1)"
            " ON CONFLICT (name) DO UPDATE SET count = test_counter.count + 1"
        )
        txn.modify(deletes=[task.as_id()])

    with psycopg.connect(pg_connstr) as conn:
        row = conn.execute("SELECT count FROM test_counter WHERE name = 'done'").fetchone()
    assert row[0] == 1
    assert eq.tasks(queue=QUEUE) == []


def test_transaction_rollback_on_dependency_error(eq: EntroQ, pg_connstr: str):
    """A DependencyError inside a transaction should roll back user SQL too."""
    QUEUE = '/test/txn_rollback'
    eq.modify(inserts=[TaskData(queue=QUEUE, value=b'item')])
    task = eq.try_claim(QUEUE)
    assert task is not None

    with pytest.raises(DependencyError):
        with eq.transaction() as txn:
            txn.conn.execute(
                "INSERT INTO test_counter (name, count) VALUES ('done', 1)"
                " ON CONFLICT (name) DO UPDATE SET count = test_counter.count + 1"
            )
            # Wrong version -- forces a DependencyError and should roll back everything.
            txn.modify(deletes=[TaskID(id=task.id, version=task.version + 99)])

    with psycopg.connect(pg_connstr) as conn:
        row = conn.execute("SELECT count FROM test_counter WHERE name = 'done'").fetchone()
    # Counter increment must have rolled back with the failed modify.
    assert row is None or row[0] == 0
