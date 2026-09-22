"""Unit tests for the async worker framework.

All tests use a fake in-process client and asyncio.run() — no Docker or
external services required.
"""
import asyncio
from datetime import datetime, timezone, timedelta

import pytest

from entroq.base import EntroQBase
from entroq.types import (
    Task, TaskID, TaskChange,
    Doc, DocID,
    DependencyError, Modification, ModifyResult, TransportError,
)
from entroq.worker import (
    StopWorker, RetryError, MoveError,
    DocClaim, Handler, EntroQWorker, default_err_q_map,
    _fix_versions, _renewing,
)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _task(id='t1', version=1, queue='q', attempt=0, claims=0, err='') -> Task:
    return Task(
        id=id, version=version, queue=queue,
        at=datetime.now(tz=timezone.utc),
        claimant='claimant', value=None,
        attempt=attempt, claims=claims, err=err,
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

    async def claim(self, queue, duration_ms=30000, poll_ms=30000, timeout_s=None):
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


class DocFailClient(FakeClient):
    """Fails claim_docs with a prescribed DependencyError."""

    def __init__(self, tasks=(), error=None):
        super().__init__(tasks=tasks)
        self._doc_error = error

    async def claim_docs(self, namespace, key, duration_ms=30000):
        raise self._doc_error


class RenewFailClient(FakeClient):
    """Fails only renewal modifies (those that just push `at` forward)."""

    def __init__(self, tasks=(), error=None):
        super().__init__(tasks=tasks)
        self._renew_error = error

    async def modify(self, modification, *, unsafe_claimant_id=None):
        changes = modification.task_changes
        is_renewal = bool(changes) and all(
            c.at is not None and c.queue == c.from_queue for c in changes
        )
        if is_renewal and self._renew_error is not None:
            raise self._renew_error
        return await super().modify(modification)


class ClaimSequenceClient(FakeClient):
    """Return tasks or raise exceptions from a prescribed claim sequence."""

    def __init__(self, sequence):
        super().__init__()
        self._claim_sequence = list(sequence)

    async def claim(self, queue, duration_ms=30000, poll_ms=30000, timeout_s=None):
        result = self._claim_sequence.pop(0)
        if isinstance(result, BaseException):
            raise result
        return result


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
    assert tc.at is None
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


def test_worker_move_error_no_queue_uses_default_map():
    """An unconfigured worker still quarantines; it must not drop the task."""
    task = _task(id='t1', version=1, queue='q')
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0, err_queue='')

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            raise MoveError("no destination")

        await worker._process(task, process)

    asyncio.run(run())

    assert client.modify_calls[0].task_changes[0].queue == 'q/err'


def test_default_err_q_map_matches_go():
    assert default_err_q_map('/my/inbox') == '/my/inbox/err'


def test_worker_error_queue_for_precedence():
    client = FakeClient()
    assert EntroQWorker(client, 'q').error_queue_for('/in') == '/in/err'
    assert EntroQWorker(client, 'q', err_queue='fixed').error_queue_for('/in') == 'fixed'
    mapped = EntroQWorker(client, 'q', err_q_map=lambda inbox: inbox + '/dead')
    assert mapped.error_queue_for('/in') == '/in/dead'


def test_worker_rejects_both_err_queue_and_map():
    with pytest.raises(ValueError):
        EntroQWorker(FakeClient(), 'q', err_queue='fixed', err_q_map=lambda q: q)


def test_worker_err_q_map_routes_per_inbox():
    """A worker watching several queues quarantines each to its own error box."""
    task = _task(id='t1', version=1, queue='/b')
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, '/a', '/b', claim_duration_s=60.0,
                          err_q_map=lambda inbox: inbox + '/quarantine')

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            raise MoveError("poison")

        await worker._process(task, process)

    asyncio.run(run())

    assert client.modify_calls[0].task_changes[0].queue == '/b/quarantine'


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


def _transport_error(*, safe_to_retry):
    cause = OSError("network down")
    return TransportError(
        "claim transport failed",
        cause=cause,
        safe_to_retry=safe_to_retry,
    )


def test_worker_retries_safe_transport_errors_with_full_jitter(monkeypatch):
    client = ClaimSequenceClient([
        _transport_error(safe_to_retry=True),
        _transport_error(safe_to_retry=True),
        _task(),
    ])
    worker = EntroQWorker(
        client,
        'q',
        transport_retry_base_s=1.0,
        transport_retry_max_s=8.0,
    )
    jitter_ranges = []

    def no_wait(lower, upper):
        jitter_ranges.append((lower, upper))
        return 0

    monkeypatch.setattr('entroq.worker.random.uniform', no_wait)

    @EntroQWorker.handler
    async def handle(task, docs):
        raise StopWorker

    asyncio.run(worker.run(handle))
    assert jitter_ranges == [(0, 1.0), (0, 2.0)]


def test_worker_resets_transport_backoff_after_a_claim(monkeypatch):
    client = ClaimSequenceClient([
        _transport_error(safe_to_retry=True),
        _task(id='first'),
        _transport_error(safe_to_retry=True),
        _task(id='last'),
    ])
    worker = EntroQWorker(client, 'q')
    jitter_ranges = []
    monkeypatch.setattr(
        'entroq.worker.random.uniform',
        lambda lower, upper: jitter_ranges.append((lower, upper)) or 0,
    )

    @EntroQWorker.handler
    async def handle(task, docs):
        if task.id == 'last':
            raise StopWorker
        return Modification(Modification.deleting(task))

    asyncio.run(worker.run(handle))
    assert jitter_ranges == [(0, 1.0), (0, 1.0)]


def test_worker_propagates_ambiguous_transport_error():
    error = _transport_error(safe_to_retry=False)
    client = ClaimSequenceClient([error])
    worker = EntroQWorker(client, 'q')

    @EntroQWorker.handler
    async def handle(task, docs):
        raise AssertionError("handler should not run")

    with pytest.raises(TransportError) as raised:
        asyncio.run(worker.run(handle))
    assert raised.value is error


def test_worker_can_delegate_safe_transport_retries_to_caller():
    error = _transport_error(safe_to_retry=True)
    client = ClaimSequenceClient([error])
    worker = EntroQWorker(client, 'q', retry_transport_errors=False)

    @EntroQWorker.handler
    async def handle(task, docs):
        raise AssertionError("handler should not run")

    with pytest.raises(TransportError) as raised:
        asyncio.run(worker.run(handle))
    assert raised.value is error


def test_worker_propagates_unexpected_handler_errors():
    client = FakeClient(tasks=[_task()])
    worker = EntroQWorker(client, 'q')

    @EntroQWorker.handler
    async def handle(task, docs):
        raise RuntimeError("application bug")

    with pytest.raises(RuntimeError, match="application bug"):
        asyncio.run(worker.run(handle))


def test_worker_rejects_invalid_transport_backoff():
    with pytest.raises(ValueError, match="non-negative"):
        EntroQWorker(FakeClient(), 'q', transport_retry_base_s=-1)
    with pytest.raises(ValueError, match="at least"):
        EntroQWorker(
            FakeClient(),
            'q',
            transport_retry_base_s=2,
            transport_retry_max_s=1,
        )


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


def _run_retry(task, *, max_attempts=0, err_queue='', exc=None):
    """Drive one RetryError through _process and return the emitted TaskChange."""
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0,
                          max_attempts=max_attempts, err_queue=err_queue)

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            raise exc

        await worker._process(task, process)

    asyncio.run(run())
    return client.modify_calls[0].task_changes[0]


def test_worker_retry_quarantines_on_final_attempt():
    """The last failure quarantines inline, not on some later claim."""
    tc = _run_retry(_task(id='t1', attempt=2), max_attempts=3, err_queue='err',
                    exc=RetryError("still broken"))
    assert tc.queue == 'err'
    assert tc.attempt == 3
    assert 'still broken' in tc.err


def test_worker_retry_below_ceiling_still_retries():
    tc = _run_retry(_task(id='t1', attempt=0), max_attempts=3, err_queue='err',
                    exc=RetryError("transient"))
    assert tc.queue == 'q'
    assert tc.at is not None
    assert tc.attempt == 1


def test_worker_quarantine_does_not_inherit_retry_delay():
    """A quarantined task is for inspection: the retry delay must not leak in."""
    tc = _run_retry(_task(id='t1', attempt=2), max_attempts=3, err_queue='err',
                    exc=RetryError("late", delay_s=600.0))
    assert tc.queue == 'err'
    assert tc.at is None


def test_worker_retry_or_move_to_overrides_quarantine_queue():
    tc = _run_retry(_task(id='t1', attempt=2), max_attempts=3, err_queue='err',
                    exc=RetryError("needs a human", or_move_to='/manual-review'))
    assert tc.queue == '/manual-review'


def test_worker_retry_or_move_to_ignored_before_ceiling():
    """or_move_to picks the quarantine queue; it must not divert a retry."""
    tc = _run_retry(_task(id='t1', attempt=0), max_attempts=3, err_queue='err',
                    exc=RetryError("transient", or_move_to='/manual-review'))
    assert tc.queue == 'q'


def test_worker_retry_without_ceiling_never_quarantines():
    """max_attempts=0 means unlimited, matching Go."""
    tc = _run_retry(_task(id='t1', attempt=99), exc=RetryError("forever"))
    assert tc.queue == 'q'
    assert tc.at is not None


def test_worker_move_error_increments_attempt():
    """Go's Quarantine increments the attempt count; Python now matches."""
    tc = _run_retry(_task(id='t1', attempt=1), err_queue='err',
                    exc=MoveError("poison"))
    assert tc.queue == 'err'
    assert tc.attempt == 2


def test_worker_max_claims_moves_task_without_running_handler():
    task = _task(id='t1', version=1, claims=4)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0, max_claims=3, err_queue='err')

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
    assert tc.at is None
    assert 'maximum claims' in tc.err


def test_worker_max_claims_unconfigured_uses_default_map():
    task = _task(id='t1', version=1, queue='q', claims=4)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0, max_claims=3)

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            raise StopWorker

        await worker._process(task, process)

    asyncio.run(run())

    assert client.modify_calls[0].task_changes[0].queue == 'q/err'


def test_worker_max_claims_allows_task_at_the_ceiling():
    """claims == max_claims is the last allowed run, matching the Go worker."""
    task = _task(id='t1', version=1, claims=3)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0, max_claims=3, err_queue='err')

    work_called = []

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            work_called.append(t.id)
            raise StopWorker

        await worker._process(task, process)

    asyncio.run(run())

    assert work_called == ['t1']
    assert client.modify_calls == []


def test_worker_max_claims_zero_means_no_limit():
    task = _task(id='t1', version=1, claims=1000)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0, err_queue='err')

    work_called = []

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            work_called.append(t.id)
            raise StopWorker

        await worker._process(task, process)

    asyncio.run(run())

    assert work_called == ['t1']


# ---------------------------------------------------------------------------
# Doc acquisition failures
# ---------------------------------------------------------------------------

def _doc_dispose(error):
    """Fail doc acquisition with `error` and return the emitted TaskChange."""
    task = _task(id='t1', version=1, queue='q')
    client = DocFailClient(tasks=[task], error=error)
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)
    work_called = []

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            work_called.append(t.id)
            return None

        @process.selector
        async def process(t):
            return [DocClaim('ns', 'k')]

        await worker._process(task, process)

    asyncio.run(run())
    assert work_called == [], "handler must not run when docs were not acquired"
    return client.modify_calls[0].task_changes[0]


def test_worker_missing_doc_is_a_poison_pill():
    """A required doc that no longer exists can never be claimed: quarantine."""
    tc = _doc_dispose(DependencyError(
        "gone", doc_depends=[DocID(namespace='ns', id='d1', version=3)]))
    assert tc.queue == 'q/err'
    assert 'required doc missing' in tc.err
    assert 'd1' in tc.err, "the error must name the doc, not just its absence"


def test_worker_contended_doc_retries_with_backoff():
    """A doc held by another claimant is transient: back off and retry."""
    tc = _doc_dispose(DependencyError(
        "held", doc_claims=[DocID(namespace='ns', id='d1', version=3)]))
    assert tc.queue == 'q'
    assert tc.at is not None
    assert 'doc contention' in tc.err
    assert tc.attempt == 1


def test_worker_missing_doc_wins_over_contention():
    """Mixed failure: an absent doc cannot be waited out, so it dominates."""
    tc = _doc_dispose(DependencyError(
        "both",
        doc_depends=[DocID(namespace='ns', id='gone', version=1)],
        doc_claims=[DocID(namespace='ns', id='held', version=1)]))
    assert tc.queue == 'q/err'


# ---------------------------------------------------------------------------
# Claim loss during work
# ---------------------------------------------------------------------------

def test_worker_cancels_handler_when_claim_is_lost():
    """Losing the claim mid-work aborts the handler instead of letting it finish.

    The handler's sleep is short enough to finish well inside the outer
    timeout, so `reached_end` staying empty means the work was genuinely
    cancelled rather than merely outlived by the test.
    """
    task = _task(id='t1', version=1, queue='q')
    client = RenewFailClient(tasks=[task], error=DependencyError("claim lost"))
    worker = EntroQWorker(client, 'q', claim_duration_s=0.02)

    reached_end = []

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            await asyncio.sleep(1.0)
            reached_end.append(t.id)
            return None

        started = asyncio.get_event_loop().time()
        with pytest.raises(DependencyError):
            await worker._process(task, process)
        return asyncio.get_event_loop().time() - started

    elapsed = asyncio.run(asyncio.wait_for(run(), timeout=10))
    assert reached_end == [], "handler kept running after the claim was gone"
    assert elapsed < 0.5, f"claim loss took {elapsed:.2f}s to stop the handler"


def test_worker_outer_cancellation_still_propagates():
    """A cancel that did not come from claim loss must not be swallowed."""
    task = _task(id='t1', version=1, queue='q')
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            await asyncio.sleep(5)
            return None

        proc = asyncio.ensure_future(worker._process(task, process))
        await asyncio.sleep(0.05)
        proc.cancel()
        with pytest.raises(asyncio.CancelledError):
            await proc

    asyncio.run(asyncio.wait_for(run(), timeout=5))


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
