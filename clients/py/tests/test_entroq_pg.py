"""Integration tests for entroq_pg.

All tests require a running Docker daemon. The pg_connstr session fixture in
conftest.py starts a throwaway PostgreSQL container and applies schema.sql
once per test session. The eq function fixture truncates the tasks table
before each test.

These tests focus on Python+native-PostgreSQL-specific behavior: LISTEN/NOTIFY
wakeup, the renewing context manager, and transaction composability with
user SQL.  Protocol-level correctness (insert/claim/delete semantics) is covered
by the Go qtest suite run against the same stored procedures.
"""

import threading
import time

import psycopg
import pytest

from entroq.pg import DependencyError, EntroQ, EQWorker, StopWorker, TaskData, TaskID


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
    eq.modify(inserts=[TaskData(queue=QUEUE, value='ping')])
    t.join(timeout=5.0)
    elapsed = time.monotonic() - insert_time

    assert not t.is_alive(), 'claim() did not unblock within 5s after insert'
    assert len(claimed) == 1
    # With NOTIFY the wakeup should be nearly immediate; 3s is very generous.
    assert elapsed < 3.0, f'claim() took {elapsed:.2f}s -- NOTIFY may not have fired'


# ---------------------------------------------------------------------------
# renewing -- renewal correctness
# ---------------------------------------------------------------------------

def test_renewal_updates_task_version(eq: EntroQ):
    """Renewer thread should bump the task version during the renewing block."""
    QUEUE = '/test/renew'
    DURATION = 1  # seconds; renewer fires at 0.5s

    eq.modify(inserts=[TaskData(queue=QUEUE, value='work')])
    task = eq.try_claim(QUEUE, duration_ms=DURATION * 1000)
    assert task is not None
    initial_version = task.version

    with eq.renewing(task, duration=DURATION) as current_task:
        time.sleep(DURATION * 1.5)  # sleep past first renewal window
        version_during = current_task().version

    # After the block, renewal has stopped and the version is stable.
    final_task = current_task()
    assert version_during > initial_version, (
        f'version should have increased after renewal '
        f'(initial={initial_version}, during={version_during})'
    )
    assert final_task.version >= version_during


# ---------------------------------------------------------------------------
# renewing -- thread safety
# ---------------------------------------------------------------------------

def test_concurrent_renewing_no_deadlock(eq: EntroQ):
    """Many concurrent renewing blocks should all complete without deadlocking."""
    N = 20
    QUEUE = '/test/concurrent_renew'
    DURATION = 1  # renewer fires at 0.5s

    eq.modify(inserts=[TaskData(queue=QUEUE, value=str(i)) for i in range(N)])

    errors = []
    results = []
    lock = threading.Lock()

    def run_one():
        task = eq.try_claim(QUEUE, duration_ms=DURATION * 1000 * 10)
        if task is None:
            return
        try:
            with eq.renewing(task, duration=DURATION) as current_task:
                time.sleep(DURATION * 1.5)  # ensure at least one renewal
                version_during = current_task().version
            final_task = current_task()
            with lock:
                results.append((final_task.id, version_during))
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
    eq.modify(inserts=[TaskData(queue=QUEUE, value='item')])
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


# ---------------------------------------------------------------------------
# Stress: concurrent workers with renewal and transactional counter
# ---------------------------------------------------------------------------

def test_concurrent_workers_transactional_counter(eq: EntroQ, pg_connstr: str):
    """Many concurrent workers claiming, renewing, and committing atomically.

    N tasks are inserted. W workers race to claim and process them, each
    deleting the task and incrementing a shared counter in a single transaction
    via renewing + transaction. The final counter must equal exactly N -- any
    double-claim, lost transaction, or renewal bug would produce the wrong value.
    """
    N = 50
    W = 10
    QUEUE = '/test/stress'
    DURATION = 2  # seconds; renewer fires at 1s

    eq.modify(inserts=[TaskData(queue=QUEUE, value=str(i)) for i in range(N)])

    errors = []
    lock = threading.Lock()

    def worker():
        while True:
            task = eq.try_claim(QUEUE, duration_ms=DURATION * 1000 * 5)
            if task is None:
                return
            try:
                with eq.renewing(task, duration=DURATION) as current_task:
                    time.sleep(DURATION * 1.2)  # trigger at least one renewal
                # Renewal stopped; current_task() is stable.
                with eq.transaction() as txn:
                    txn.conn.execute(
                        "INSERT INTO test_counter (name, count) VALUES ('done', 1)"
                        " ON CONFLICT (name) DO UPDATE SET count = test_counter.count + 1"
                    )
                    txn.modify(deletes=[current_task().as_id()])
            except Exception as exc:
                with lock:
                    errors.append(exc)
                return

    threads = [threading.Thread(target=worker) for _ in range(W)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=120)

    still_alive = sum(1 for t in threads if t.is_alive())
    assert still_alive == 0, f'{still_alive} worker threads still running -- possible deadlock'
    assert not errors, f'exceptions in worker threads: {errors}'

    with psycopg.connect(pg_connstr) as conn:
        row = conn.execute("SELECT count FROM test_counter WHERE name = 'done'").fetchone()
    final_count = row[0] if row else 0
    assert final_count == N, f'expected counter={N}, got {final_count} -- possible double-claim or lost transaction'
    assert eq.tasks(queue=QUEUE) == [], 'tasks remain in queue after all workers finished'


def test_concurrent_workers_multi_queue(eq: EntroQ, pg_connstr: str):
    """Workers claiming from multiple queues simultaneously, with renewal and transactional counter.

    Tasks are spread across Q queues. Workers each listen on all queues and
    race to claim from any of them. Final counter must equal exactly N --
    verifying that multi-queue claiming, LISTEN/NOTIFY across multiple channels,
    renewal, and transactional atomicity all work correctly under concurrent load.
    """
    N = 50          # total tasks
    W = 10          # workers
    Q = 5           # queues
    DURATION = 2    # seconds; renewer fires at 1s
    QUEUES = [f'/test/multi/{i}' for i in range(Q)]

    # Spread tasks across queues round-robin.
    eq.modify(inserts=[
        TaskData(queue=QUEUES[i % Q], value=str(i))
        for i in range(N)
    ])

    errors = []
    lock = threading.Lock()

    def worker():
        while True:
            task = eq.try_claim(QUEUES, duration_ms=DURATION * 1000 * 5)
            if task is None:
                return
            try:
                with eq.renewing(task, duration=DURATION) as current_task:
                    time.sleep(DURATION * 1.2)  # trigger at least one renewal
                # Renewal stopped; current_task() is stable.
                with eq.transaction() as txn:
                    txn.conn.execute(
                        "INSERT INTO test_counter (name, count) VALUES ('done', 1)"
                        " ON CONFLICT (name) DO UPDATE SET count = test_counter.count + 1"
                    )
                    txn.modify(deletes=[current_task().as_id()])
            except Exception as exc:
                with lock:
                    errors.append(exc)
                return

    threads = [threading.Thread(target=worker) for _ in range(W)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=120)

    still_alive = sum(1 for t in threads if t.is_alive())
    assert still_alive == 0, f'{still_alive} worker threads still running -- possible deadlock'
    assert not errors, f'exceptions in worker threads: {errors}'

    with psycopg.connect(pg_connstr) as conn:
        row = conn.execute("SELECT count FROM test_counter WHERE name = 'done'").fetchone()
    final_count = row[0] if row else 0
    assert final_count == N, f'expected counter={N}, got {final_count}'
    for q in QUEUES:
        assert eq.tasks(queue=q) == [], f'tasks remain in queue {q}'


def test_multi_queue_fairness(eq: EntroQ):
    """Multi-queue try_claim should drain smaller queues proportionally early.

    With fair random queue selection across 3 queues, the smallest queue (20
    tasks) should be exhausted well before the first third of total consumption,
    and the medium queue (60 tasks) before the first three-quarters. A broken
    implementation that always tries the first queue first would exhaust small
    and med near the very end.
    """
    BIG_SIZE = 300
    MED_SIZE = 60
    SMALL_SIZE = 20
    N_WORKERS = 5

    big_q   = '/test/fairness/big'
    med_q   = '/test/fairness/med'
    small_q = '/test/fairness/small'
    queues  = [big_q, med_q, small_q]

    eq.modify(inserts=(
        [TaskData(queue=big_q,   value='x') for _ in range(BIG_SIZE)] +
        [TaskData(queue=med_q,   value='x') for _ in range(MED_SIZE)] +
        [TaskData(queue=small_q, value='x') for _ in range(SMALL_SIZE)]
    ))

    consumed = []
    errors = []
    lock = threading.Lock()

    def worker():
        while True:
            try:
                task = eq.claim(queues, timeout_s=3)
            except TimeoutError:
                with lock:
                    remaining = sum(1 for q in queues if eq.tasks(queue=q))
                    if remaining:
                        errors.append(RuntimeError(
                            f'claim timed out with {remaining} tasks still in queues '
                            f'(consumed so far: {len(consumed)})'
                        ))
                return
            with lock:
                consumed.append(task.queue)
            try:
                eq.modify(deletes=[task.as_id()])
            except Exception as exc:
                with lock:
                    errors.append(exc)
                return

    threads = [threading.Thread(target=worker) for _ in range(N_WORKERS)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=60)

    assert sum(1 for t in threads if t.is_alive()) == 0, 'worker threads timed out'
    assert not errors, f'worker errors: {errors}'
    assert len(consumed) == BIG_SIZE + MED_SIZE + SMALL_SIZE, (
        f'expected {BIG_SIZE + MED_SIZE + SMALL_SIZE} tasks consumed, got {len(consumed)}'
    )

    total = len(consumed)
    last_small = max((i for i, q in enumerate(consumed) if q == small_q), default=-1)
    last_med   = max((i for i, q in enumerate(consumed) if q == med_q),   default=-1)

    # Small (20 tasks): expected to drain after ~60 total claims (1/3 of
    # traffic). Allow up to total//3 as a generous upper bound.
    assert last_small < total // 3, (
        f'small queue not exhausted fairly: last task at position {last_small}/{total} '
        f'(threshold {total // 3})'
    )
    # Med (60 tasks): expected to drain after ~140 total claims. Allow 3/4.
    assert last_med < total * 3 // 4, (
        f'med queue not exhausted fairly: last task at position {last_med}/{total} '
        f'(threshold {total * 3 // 4})'
    )


def test_transaction_rollback_on_dependency_error(eq: EntroQ, pg_connstr: str):
    """A DependencyError inside a transaction should roll back user SQL too."""
    QUEUE = '/test/txn_rollback'
    eq.modify(inserts=[TaskData(queue=QUEUE, value='item')])
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


# ---------------------------------------------------------------------------
# EQWorker
# ---------------------------------------------------------------------------

def test_worker_processes_tasks(eq: EntroQ):
    """Worker should claim and complete N tasks via renew+finalize."""
    N = 5
    QUEUE = '/test/worker/basic'

    eq.modify(inserts=[TaskData(queue=QUEUE, value=str(i)) for i in range(N)])

    completed = []

    def handle(renew, finalize):
        with renew as task:
            value = task.value
        with finalize as stable_task:
            completed.append(value)
            eq.modify(deletes=[stable_task.as_id()])
        if len(completed) >= N:
            raise StopWorker

    w = EQWorker(eq)
    t = threading.Thread(target=w.work, args=(QUEUE, handle), daemon=True)
    t.start()
    t.join(timeout=10)

    assert not t.is_alive(), 'worker did not stop within timeout'
    assert len(completed) == N
    assert eq.tasks(queue=QUEUE) == []


def test_worker_stable_version_in_finalize(eq: EntroQ):
    """Version yielded by finalize should be >= version seen during renew."""
    QUEUE = '/test/worker/version'
    DURATION = 1  # renewer fires at 0.5s

    eq.modify(inserts=[TaskData(queue=QUEUE, value='work')])

    versions = {}

    def handle(renew, finalize):
        with renew as task:
            versions['during'] = task.version
            time.sleep(DURATION * 1.5)  # trigger at least one renewal
        with finalize as stable_task:
            versions['final'] = stable_task.version
            eq.modify(deletes=[stable_task.as_id()])
        raise StopWorker

    w = EQWorker(eq)
    t = threading.Thread(target=w.work, args=(QUEUE, handle),
                         kwargs={'claim_duration': DURATION}, daemon=True)
    t.start()
    t.join(timeout=10)

    assert not t.is_alive(), 'worker did not stop within timeout'
    assert 'during' in versions and 'final' in versions
    assert versions['final'] >= versions['during'], (
        f"final version {versions['final']} should be >= during version {versions['during']}"
    )


def test_worker_continues_after_dependency_error(eq: EntroQ):
    """Worker should log and continue when do_func raises a DependencyError."""
    QUEUE = '/test/worker/dep_err'

    eq.modify(inserts=[TaskData(queue=QUEUE, value='work')])

    outcomes = []
    calls = [0]

    def handle(renew, finalize):
        with renew:
            pass
        calls[0] += 1
        if calls[0] == 1:
            # Simulate a dependency error on the first attempt.
            raise DependencyError('simulated')
        with finalize as stable_task:
            outcomes.append(stable_task.value)
            eq.modify(deletes=[stable_task.as_id()])
        raise StopWorker

    w = EQWorker(eq)
    t = threading.Thread(target=w.work, args=(QUEUE, handle),
                         kwargs={'claim_duration': 2}, daemon=True)
    t.start()
    t.join(timeout=20)

    assert not t.is_alive(), 'worker did not stop within timeout'
    assert calls[0] == 2, f'expected 2 calls (1 dep error + 1 success), got {calls[0]}'
    assert outcomes == ['work']
