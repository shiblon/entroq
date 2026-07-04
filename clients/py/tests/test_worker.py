"""Unit tests for the async worker framework.

All tests use a fake in-process client and asyncio.run() — no Docker or
external services required.
"""
import asyncio
from datetime import datetime, timezone, timedelta

from entroq.base import EntroQBase
from entroq.types import (
    Task, TaskID, TaskChange,
    Doc, DocID,
    DependencyError, Modification, ModifyResult,
)
from entroq.worker import (
    StopWorker, RetryError, MoveError,
    DocClaim, Handler, EntroQWorker,
    _fix_versions, _renewing,
)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _task(id='t1', version=1, queue='q', attempt=0, err='') -> Task:
    return Task(
        id=id, version=version, queue=queue,
        at=datetime.now(tz=timezone.utc),
        claimant='claimant', value=None,
        attempt=attempt, err=err,
    )


def _doc(namespace='ns', id='d1', version=1, key='k', secondary_key='') -> Doc:
    return Doc(
        namespace=namespace, id=id, version=version,
        key=key, secondary_key=secondary_key,
        content=None, claimant='claimant',
        at=datetime.now(tz=timezone.utc),
    )


class FakeClient(EntroQBase):
    """Controllable in-process client for worker tests."""

    def __init__(self, tasks=(), docs=()):
        self._task_q: asyncio.Queue[Task] = asyncio.Queue()
        self._docs = list(docs)
        self.modify_calls: list[Modification] = []
        self.modify_raises: Exception | None = None
        for t in tasks:
            self._task_q.put_nowait(t)

    async def time(self) -> datetime:
        return datetime.now(tz=timezone.utc)

    async def queues(self, prefix='', exact=(), limit=0):
        return []

    async def tasks(self, queue='', limit=0, omit_values=False):
        return []

    async def try_claim(self, queue, duration_ms=30000):
        try:
            return self._task_q.get_nowait()
        except asyncio.QueueEmpty:
            return None

    async def claim(self, queue, duration_ms=30000, poll_ms=5000, timeout_s=None):
        return await self._task_q.get()

    async def modify(self, modification, *, unsafe_claimant_id=None):
        if self.modify_raises is not None:
            raise self.modify_raises
        self.modify_calls.append(modification)
        tasks_changed = [
            Task(id=tc.id, version=tc.version + 1, queue=tc.queue,
                 at=tc.at or datetime.now(tz=timezone.utc),
                 claimant='claimant', value=tc.value,
                 attempt=tc.attempt, err=tc.err or '')
            for tc in modification.task_changes
        ]
        docs_changed = [
            Doc(namespace=dc.namespace, id=dc.id, version=dc.version + 1,
                key=dc.key, secondary_key=dc.secondary_key,
                content=dc.content, claimant='claimant',
                at=dc.at or datetime.now(tz=timezone.utc))
            for dc in modification.doc_changes
        ]
        return ModifyResult(tasks_changed=tasks_changed, docs_changed=docs_changed)

    async def docs(self, namespace='', key_start='', key_end='', limit=0, omit_values=False):
        return list(self._docs)

    async def claim_docs(self, namespace, key, duration_ms=30000):
        return [d for d in self._docs if d.namespace == namespace and d.key == key]


# ---------------------------------------------------------------------------
# _fix_versions — pure unit tests
# ---------------------------------------------------------------------------

def test_fix_versions_task_change():
    task = _task(id='t1', version=5)
    tc = TaskChange(id='t1', version=1, queue='q', at=task.at)
    mod = Modification()
    mod.task_changes.append(tc)
    _fix_versions(mod, task, [])
    assert mod.task_changes[0].version == 5


def test_fix_versions_task_delete():
    task = _task(id='t1', version=5)
    mod = Modification(Modification.deleting(TaskID(id='t1', version=1, queue='q')))
    _fix_versions(mod, task, [])
    assert mod.task_deletes[0].version == 5


def test_fix_versions_task_depend():
    task = _task(id='t1', version=5)
    mod = Modification(Modification.depending(TaskID(id='t1', version=1, queue='q')))
    _fix_versions(mod, task, [])
    assert mod.task_depends[0].version == 5


def test_fix_versions_doc_change():
    doc = _doc(namespace='ns', id='d1', version=7)
    mod = Modification()
    dc = doc.as_change()
    dc.version = 2
    mod.doc_changes.append(dc)
    _fix_versions(mod, _task(), [doc])
    assert mod.doc_changes[0].version == 7


def test_fix_versions_doc_delete():
    doc = _doc(namespace='ns', id='d1', version=7)
    mod = Modification(Modification.deleting(DocID(namespace='ns', id='d1', version=2)))
    _fix_versions(mod, _task(), [doc])
    assert mod.doc_deletes[0].version == 7


def test_fix_versions_skips_unknown_ids():
    task = _task(id='t1', version=5)
    tc = TaskChange(id='t-other', version=3, queue='q', at=task.at)
    mod = Modification()
    mod.task_changes.append(tc)
    _fix_versions(mod, task, [])
    assert mod.task_changes[0].version == 3  # unchanged


# ---------------------------------------------------------------------------
# @EntroQWorker.handler decorator and chaining
# ---------------------------------------------------------------------------

def test_handler_decorator_creates_fn_handler():
    client = FakeClient()
    worker = EntroQWorker(client, 'q')

    @EntroQWorker.handler
    async def process(task, docs):
        return None

    assert isinstance(process, Handler)


def test_handler_selector_chaining():
    @EntroQWorker.handler
    async def process(task, docs):
        return None

    @process.selector
    async def process(task):
        return [DocClaim('ns', 'k', duration_s=10.0)]

    assert isinstance(process, Handler)
    claims = asyncio.run(process._select(_task()))
    assert len(claims) == 1
    assert claims[0].namespace == 'ns'
    assert claims[0].key == 'k'


def test_handler_finisher_chaining():
    client = FakeClient()
    worker = EntroQWorker(client, 'q')

    finished = []

    @EntroQWorker.handler
    async def process(task, docs):
        return None

    @process.finisher
    async def process(task, docs):
        finished.append(task.id)

    asyncio.run(process._do_finish(_task(), []))
    assert finished == ['t1']


def test_handler_all_three_chained():
    @EntroQWorker.handler
    async def process(task, docs):
        return Modification(Modification.deleting(task))

    @process.selector
    async def process(task):
        return [DocClaim('ns', 'k')]

    @process.finisher
    async def process(task, docs):
        pass

    assert isinstance(process, Handler)
    assert len(asyncio.run(process._select(_task()))) == 1


def test_handler_chaining_is_immutable():
    """Each chaining step returns a new object; the original is unchanged."""
    @EntroQWorker.handler
    async def process(task, docs):
        return None

    original = process

    @process.selector
    async def process(task):
        return [DocClaim('ns', 'k')]

    assert asyncio.run(original._select(_task())) == []  # original unchanged
    assert len(asyncio.run(process._select(_task()))) == 1


# ---------------------------------------------------------------------------
# Worker normal flow
# ---------------------------------------------------------------------------

def test_worker_applies_modification_with_fixed_versions():
    task = _task(id='t1', version=1)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            return Modification(Modification.deleting(t))

        await worker._process(task, process)

    asyncio.run(run())

    assert len(client.modify_calls) == 1
    assert len(client.modify_calls[0].task_deletes) == 1
    assert client.modify_calls[0].task_deletes[0].id == 't1'


def test_worker_stop_worker_stops_loop():
    tasks = [_task(id=f't{i}') for i in range(5)]
    client = FakeClient(tasks=tasks)
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)

    processed = []

    async def run():
        @EntroQWorker.handler
        async def process(task, docs):
            processed.append(task.id)
            raise StopWorker

        await worker.run(process)

    asyncio.run(run())
    assert len(processed) == 1
    assert len(client.modify_calls) == 0


def test_worker_retry_error_requeues_with_delay():
    task = _task(id='t1', version=1, attempt=0)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0, retry_delay_s=45.0)

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            raise RetryError("transient failure")

        await worker._process(task, process)

    asyncio.run(run())

    assert len(client.modify_calls) == 1
    tc = client.modify_calls[0].task_changes[0]
    assert tc.id == 't1'
    assert tc.attempt == 1
    assert 'transient failure' in tc.err
    assert tc.at > datetime.now(tz=timezone.utc)


def test_worker_retry_error_custom_delay():
    task = _task(id='t1', version=1)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0, retry_delay_s=99.0)

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            raise RetryError("oops", delay_s=10.0)

        await worker._process(task, process)

    asyncio.run(run())

    tc = client.modify_calls[0].task_changes[0]
    assert tc.at < datetime.now(tz=timezone.utc) + timedelta(seconds=20)


def test_worker_move_error_changes_queue():
    task = _task(id='t1', version=1, queue='src')
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'src', claim_duration_s=60.0)

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            raise MoveError("bad task", queue='error-queue')

        await worker._process(task, process)

    asyncio.run(run())

    tc = client.modify_calls[0].task_changes[0]
    assert tc.queue == 'error-queue'
    assert 'bad task' in tc.err


def test_worker_move_error_uses_err_queue():
    task = _task(id='t1', version=1, queue='src')
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'src', claim_duration_s=60.0, err_queue='global-err')

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            raise MoveError("poison")

        await worker._process(task, process)

    asyncio.run(run())

    assert client.modify_calls[0].task_changes[0].queue == 'global-err'


def test_worker_move_error_no_queue_no_modify():
    task = _task(id='t1', version=1)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0, err_queue='')

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            raise MoveError("no destination")

        await worker._process(task, process)

    asyncio.run(run())

    assert len(client.modify_calls) == 0


def test_worker_stop_event_exits_after_current_task():
    tasks = [_task(id=f't{i}') for i in range(5)]
    client = FakeClient(tasks=tasks)
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)

    processed = []

    async def run():
        @EntroQWorker.handler
        async def process(task, docs):
            processed.append(task.id)
            worker.stop()
            return Modification(Modification.deleting(task))

        await worker.run(process)

    asyncio.run(run())
    assert len(processed) == 1


def test_worker_stop_unblocks_waiting_claim():
    """stop() must exit the loop even when claim() is blocking on an empty queue."""
    client = FakeClient()  # empty — claim() will block
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)

    processed = []

    async def run():
        @EntroQWorker.handler
        async def process(task, docs):
            processed.append(task.id)  # should never be called
            raise StopWorker

        async def stopper():
            await asyncio.sleep(0.05)
            worker.stop()

        await asyncio.gather(worker.run(process), stopper())

    asyncio.run(run())
    assert processed == []


def test_worker_dep_error_logs_and_continues():
    task_a = _task(id='ta', version=1)
    task_b = _task(id='tb', version=1)
    client = FakeClient(tasks=[task_a, task_b])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)

    processed = []

    async def run():
        @EntroQWorker.handler
        async def process(task, docs):
            processed.append(task.id)
            if task.id == 'ta':
                raise DependencyError("simulated")
            raise StopWorker

        await worker.run(process)

    asyncio.run(run())
    assert 'ta' in processed
    assert 'tb' in processed


def test_worker_finisher_called_when_do_work_returns_none():
    task = _task(id='t1', version=1)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)

    finished = []

    async def run():
        class MyHandler(Handler):
            async def do_work(self, t, docs):
                return None

            async def finish(self, t, docs):
                finished.append(t.id)

        await worker._process(task, MyHandler())

    asyncio.run(run())
    assert finished == ['t1']


def test_worker_finisher_called_even_when_do_work_returns_modification():
    task = _task(id='t1', version=1)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)

    finished = []

    async def run():
        class MyHandler(Handler):
            async def do_work(self, t, docs):
                return Modification(Modification.deleting(t))

            async def finish(self, t, docs):
                finished.append(t.id)

        await worker._process(task, MyHandler())

    asyncio.run(run())
    assert finished == ['t1']


def test_worker_max_attempts_moves_task():
    task = _task(id='t1', version=1, attempt=3)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0, max_attempts=3, err_queue='err')

    work_called = []

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            work_called.append(t.id)
            raise StopWorker

        await worker._process(task, process)

    asyncio.run(run())

    assert work_called == []
    tc = client.modify_calls[0].task_changes[0]
    assert tc.queue == 'err'
    assert 'max attempts' in tc.err


# ---------------------------------------------------------------------------
# Doc claiming
# ---------------------------------------------------------------------------

def test_worker_claims_docs_for_task():
    task = _task(id='t1', version=1)
    doc = _doc(namespace='ns', id='d1', version=1, key='mykey')
    client = FakeClient(tasks=[task], docs=[doc])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)

    received_docs = []

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            received_docs.extend(docs)
            return Modification(Modification.deleting(t))

        @process.selector
        async def process(task):
            return [DocClaim('ns', 'mykey')]

        await worker._process(task, process)

    asyncio.run(run())
    assert len(received_docs) == 1
    assert received_docs[0].id == 'd1'


def test_worker_doc_claims_sorted_by_namespace_key():
    """Docs must be claimed in (namespace, key) order to avoid livelock."""
    task = _task(id='t1', version=1)
    doc_a = _doc(namespace='ns', id='da', version=1, key='aaa')
    doc_b = _doc(namespace='ns', id='db', version=1, key='bbb')
    client = FakeClient(tasks=[task], docs=[doc_a, doc_b])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)

    claim_order = []
    original_claim_docs = client.claim_docs

    async def tracking_claim_docs(namespace, key, duration_ms=30000):
        claim_order.append((namespace, key))
        return await original_claim_docs(namespace, key, duration_ms)

    client.claim_docs = tracking_claim_docs

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            return Modification(Modification.deleting(t))

        @process.selector
        async def process(task):
            return [DocClaim('ns', 'bbb'), DocClaim('ns', 'aaa')]  # reversed

        await worker._process(task, process)

    asyncio.run(run())
    assert claim_order == [('ns', 'aaa'), ('ns', 'bbb')]


# ---------------------------------------------------------------------------
# Renewal
# ---------------------------------------------------------------------------

def test_renewing_updates_state_task():
    task = _task(id='t1', version=1)
    client = FakeClient()

    async def run():
        async with _renewing(client, task, [], duration_s=0.1) as state:
            await asyncio.sleep(0.25)
        return state.task.version

    assert asyncio.run(run()) >= 3


def test_renewing_dep_error_sets_state_error():
    task = _task(id='t1', version=1)
    client = FakeClient()
    client.modify_raises = DependencyError("lease lost")

    async def run():
        async with _renewing(client, task, [], duration_s=0.1) as state:
            await asyncio.sleep(0.2)
        return state.error

    assert isinstance(asyncio.run(run()), DependencyError)


# ---------------------------------------------------------------------------
# Ambient garbage collection
#
# The worker drives GC only when its client exposes gc_collect (the direct
# PostgreSQL backend). Talking to a Go server, the client lacks that capability
# and the server GCs itself, so the worker must stay out of the way.
# ---------------------------------------------------------------------------

class GCFakeClient(FakeClient):
    """FakeClient that also exposes the gc_collect capability, and counts it."""

    def __init__(self, *args, gc_returns=(0,), **kwargs):
        super().__init__(*args, **kwargs)
        self.gc_calls = 0
        self._gc_returns = list(gc_returns)

    async def gc_collect(self, batch=1000):
        self.gc_calls += 1
        return self._gc_returns.pop(0) if self._gc_returns else 0


def test_worker_no_gc_without_capability():
    """A client lacking gc_collect must not trip up run(); no GC is attempted."""
    client = FakeClient()
    assert not hasattr(client, 'gc_collect')  # gate is off for non-pg clients

    worker = EntroQWorker(client, 'q')
    client._task_q.put_nowait(_task())

    @EntroQWorker.handler
    async def handle(task, docs):
        worker.stop()
        return Modification(Modification.deleting(task))

    asyncio.run(worker.run(handle))  # completes cleanly, no missing-attr error


def test_worker_drives_gc_when_capable(monkeypatch):
    """A gc_collect-capable client gets its GC loop driven while the worker runs."""
    import entroq.worker as w
    monkeypatch.setattr(w, '_GC_INTERVAL_S', 0.01)

    client = GCFakeClient()
    worker = EntroQWorker(client, 'q')

    @EntroQWorker.handler
    async def handle(task, docs):
        return Modification(Modification.deleting(task))

    async def drive():
        run_task = asyncio.create_task(worker.run(handle))
        for _ in range(200):  # up to ~2s for at least one GC pass
            if client.gc_calls >= 1:
                break
            await asyncio.sleep(0.01)
        worker.stop()
        await run_task

    asyncio.run(drive())
    assert client.gc_calls >= 1


def test_gc_loop_tight_drains_full_batches(monkeypatch):
    """One pass keeps collecting while batches come back full, stopping on a short one."""
    import entroq.worker as w
    monkeypatch.setattr(w, '_GC_INTERVAL_S', 0.01)

    # Two full batches then a short one: the inner drain should make all three
    # calls in a single pass before idling.
    client = GCFakeClient(gc_returns=[w._GC_BATCH, w._GC_BATCH, 3])
    worker = EntroQWorker(client, 'q')

    async def drive():
        loop = asyncio.create_task(worker._gc_loop())
        for _ in range(200):
            if client.gc_calls >= 3:
                break
            await asyncio.sleep(0.005)
        worker._stop_event.set()
        await loop

    asyncio.run(drive())
    assert client.gc_calls >= 3


def test_gc_loop_survives_errors(monkeypatch):
    """A failed gc_collect is logged and retried, never fatal to the loop."""
    import entroq.worker as w
    monkeypatch.setattr(w, '_GC_INTERVAL_S', 0.01)

    class BoomThenOK(GCFakeClient):
        async def gc_collect(self, batch=1000):
            self.gc_calls += 1
            if self.gc_calls == 1:
                raise RuntimeError("transient db blip")
            return 0

    client = BoomThenOK()
    worker = EntroQWorker(client, 'q')

    async def drive():
        loop = asyncio.create_task(worker._gc_loop())
        for _ in range(200):
            if client.gc_calls >= 2:  # recovered and ran again after the error
                break
            await asyncio.sleep(0.01)
        worker._stop_event.set()
        await loop

    asyncio.run(drive())
    assert client.gc_calls >= 2
