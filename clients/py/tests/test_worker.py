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
    ClaimedDocs, Doc, DocData, DocID, DocClaim,
    DependencyError, Modification, ModifyResult, TransportError,
)
from entroq.worker import (
    StopWorker, FatalWorker, RetryError, MoveError,
    DocClaim, Handler, EntroQWorker, default_err_q_map,
    _fix_versions, _renewing,
)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _task(id='t1', version=1, queue='q', attempt=0, claims=0, err='',
          lease_s=None) -> Task:
    """Build a task. lease_s is the lease it was granted: at minus modified.

    Leaving it None leaves modified unset, which is what a task built by hand
    looks like; the worker then falls back to its default claim duration.
    """
    now = datetime.now(tz=timezone.utc)
    return Task(
        id=id, version=version, queue=queue,
        at=now if lease_s is None else now + timedelta(seconds=lease_s),
        modified=None if lease_s is None else now,
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
        # A set is a map from secondary key to doc, so a fixture that seeds two
        # docs at one place describes a set the service would refuse. Caught
        # here so a unit test cannot pass on state that cannot exist.
        places = [(d.namespace, d.key, d.secondary_key) for d in self._docs]
        if len(set(places)) != len(places):
            raise ValueError(f"seeded docs share a place in their set: {places}")
        # A doc set holds its own lock, versioned apart from its members.
        self._set_versions: dict[tuple[str, str], int] = {}
        self.claim_set_calls: list[tuple[list[DocClaim], Task | None]] = []
        for t in tasks:
            self._task_q.put_nowait(t)

    def _set_ref(self, namespace, key) -> Doc:
        """Return a set's lock as the service reports it: a doc with no id."""
        now = datetime.now(tz=timezone.utc)
        return Doc(
            namespace=namespace, id='', version=self._set_versions.setdefault(
                (namespace, key), 1),
            key=key, secondary_key='', content=None, claimant='claimant',
            at=now, modified=now)

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
        now = datetime.now(tz=timezone.utc)
        tasks_changed = [
            Task(id=tc.id, version=tc.version + 1, queue=tc.queue,
                 at=tc.at or now,
                 claimant='claimant', value=tc.value,
                 attempt=tc.attempt, err=tc.err or '')
            for tc in modification.task_changes
        ]
        # An arrival moves its item one version and sets only when the item is
        # ready again, resolved here against this client's clock as a backend
        # resolves it against its own.
        tasks_changed.extend(
            Task(id=a.id, version=a.version + 1, queue=a.queue,
                 at=now + timedelta(seconds=a.by_s), modified=now,
                 claimant='claimant', value=None)
            for a in modification.task_arrivals
        )
        docs_changed = [
            Doc(namespace=dc.namespace, id=dc.id, version=dc.version + 1,
                key=dc.key, secondary_key=dc.secondary_key,
                content=dc.content, claimant='claimant',
                at=dc.at or now)
            for dc in modification.doc_changes
        ]
        for a in modification.doc_arrivals:
            version = self._set_versions.get((a.namespace, a.key), a.version) + 1
            self._set_versions[(a.namespace, a.key)] = version
            docs_changed.append(Doc(
                namespace=a.namespace, id='', version=version, key=a.key,
                secondary_key='', content=None, claimant='claimant',
                at=now + timedelta(seconds=a.by_s), modified=now))
        return ModifyResult(tasks_changed=tasks_changed, docs_changed=docs_changed)

    async def docs(self, namespace='', key_start='', key_end='', limit=0, omit_values=False):
        return list(self._docs)

    async def claim_docs(self, namespace, key, duration_ms=30000):
        return await self.claim_doc_sets(
            [DocClaim(namespace, key)], duration_ms=duration_ms)

    async def claim_doc_sets(self, sets, *, duration_ms=30000, task_to_match=None):
        sets = list(sets)
        self.claim_set_calls.append((sets, task_to_match))
        members, refs = [], []
        for c in sets:
            members.extend(d for d in self._docs
                           if d.namespace == c.namespace and d.key == c.key)
            refs.append(self._set_ref(c.namespace, c.key))
        return ClaimedDocs(members, refs)


class DocFailClient(FakeClient):
    """Fails a doc claim with a prescribed DependencyError."""

    def __init__(self, tasks=(), error=None):
        super().__init__(tasks=tasks)
        self._doc_error = error

    async def claim_doc_sets(self, sets, *, duration_ms=30000, task_to_match=None):
        raise self._doc_error


class ParkedRenewClient(FakeClient):
    """Applies a renewal, then takes time to answer.

    A server that has already committed while its reply is still on the wire.
    renewal_applied fires the moment the write lands, so a caller waiting on it
    is guaranteed to be inside the reply's flight.
    """

    IN_FLIGHT_S = 0.05

    def __init__(self, tasks=(), docs=()):
        super().__init__(tasks=tasks, docs=docs)
        self.renewal_applied = asyncio.Event()
        self.renewed: ModifyResult | None = None

    async def modify(self, modification, *, unsafe_claimant_id=None):
        if not modification.task_arrivals:
            return await super().modify(modification)
        result = await super().modify(modification)
        self.renewed = result
        self.renewal_applied.set()
        await asyncio.sleep(self.IN_FLIGHT_S)
        return result


class SilentRenewClient(FakeClient):
    """Answers a renewal without reporting what it changed.

    Whatever it did, the caller cannot know the versions it now holds.
    """

    def __init__(self, tasks=(), docs=(), omit='task'):
        super().__init__(tasks=tasks, docs=docs)
        self._omit = omit
        self.renewals = 0

    async def modify(self, modification, *, unsafe_claimant_id=None):
        if not modification.task_arrivals:
            return await super().modify(modification)
        self.renewals += 1
        result = await super().modify(modification)
        if self._omit == 'task':
            return ModifyResult(tasks_changed=[], docs_changed=result.docs_changed)
        return ModifyResult(tasks_changed=result.tasks_changed, docs_changed=[])


class SlowClaimClient(FakeClient):
    """Takes time to claim doc sets, as a contended claim does.

    claim_elapsed_s is how long the claim took, which is lease time already
    spent when the handler body finally starts.
    """

    def __init__(self, tasks=(), docs=(), claim_delay_s=0.0):
        super().__init__(tasks=tasks, docs=docs)
        self._claim_delay_s = claim_delay_s
        self.renewed_at: list[float] = []
        self._body_start: float | None = None

    async def claim_doc_sets(self, sets, *, duration_ms=30000, task_to_match=None):
        await asyncio.sleep(self._claim_delay_s)
        result = await super().claim_doc_sets(
            sets, duration_ms=duration_ms, task_to_match=task_to_match)
        self._body_start = asyncio.get_running_loop().time()
        return result

    async def modify(self, modification, *, unsafe_claimant_id=None):
        if modification.task_arrivals and self._body_start is not None:
            self.renewed_at.append(
                asyncio.get_running_loop().time() - self._body_start)
        return await super().modify(modification)


class RenewFailClient(FakeClient):
    """Fails only renewal modifies: those carrying a task arrival."""

    def __init__(self, tasks=(), error=None):
        super().__init__(tasks=tasks)
        self._renew_error = error

    async def modify(self, modification, *, unsafe_claimant_id=None):
        if modification.task_arrivals and self._renew_error is not None:
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
    _fix_versions(mod, task, [], [])
    assert mod.task_changes[0].version == 5


def test_fix_versions_task_delete():
    task = _task(id='t1', version=5)
    mod = Modification(Modification.deleting(TaskID(id='t1', version=1, queue='q')))
    _fix_versions(mod, task, [], [])
    assert mod.task_deletes[0].version == 5


def test_fix_versions_task_depend():
    task = _task(id='t1', version=5)
    mod = Modification(Modification.depending(TaskID(id='t1', version=1, queue='q')))
    _fix_versions(mod, task, [], [])
    assert mod.task_depends[0].version == 5


def test_fix_versions_doc_change():
    """With no sets reported, a member falls back to the version it was claimed at."""
    doc = _doc(namespace='ns', id='d1', version=7)
    mod = Modification()
    dc = doc.as_change()
    dc.version = 2
    mod.doc_changes.append(dc)
    _fix_versions(mod, _task(), [doc], [])
    assert mod.doc_changes[0].version == 7


def test_fix_versions_doc_delete():
    doc = _doc(namespace='ns', id='d1', version=7)
    mod = Modification(Modification.deleting(DocID(namespace='ns', id='d1', version=2)))
    _fix_versions(mod, _task(), [doc], [])
    assert mod.doc_deletes[0].version == 7


def test_fix_versions_imposes_the_sets_version_on_its_members():
    """A member named by ID takes its set's current version, not its own.

    A doc carries its set's version and no other, so a renewal that moved the
    set's lock has moved every member with it. The members are left as they
    were claimed and the set is the one authority, applied here.
    """
    doc = _doc(namespace='ns', id='d1', key='k', version=7)
    group = _doc(namespace='ns', id='', key='k', version=9)  # renewed twice
    mod = Modification(
        Modification.deleting(DocID(namespace='ns', id='d1', version=7)),
        Modification.depending(DocID(namespace='ns', id='d1', version=7)),
    )
    change = doc.as_change()
    change.version = 7
    mod.doc_changes.append(change)

    _fix_versions(mod, _task(), [doc], [group])

    assert mod.doc_deletes[0].version == 9, "delete takes the set's version"
    assert mod.doc_depends[0].version == 9, "so does a depend"
    assert mod.doc_changes[0].version == 9, "and a change"


def test_fix_versions_leaves_docs_of_unclaimed_sets_alone():
    """A doc whose set was not claimed here keeps what the caller named.

    The leasehold decides what may be corrected; a set this worker never held
    could have moved for any reason, and guessing at its version would turn a
    dependency check into a blind write.
    """
    mine = _doc(namespace='ns', id='d1', key='k', version=7)
    group = _doc(namespace='ns', id='', key='k', version=9)
    mod = Modification(
        Modification.deleting(DocID(namespace='ns', id='elsewhere', version=3)))

    _fix_versions(mod, _task(), [mine], [group])

    assert mod.doc_deletes[0].version == 3, "untouched: not a member of a claimed set"


def test_fix_versions_skips_unknown_ids():
    task = _task(id='t1', version=5)
    tc = TaskChange(id='t-other', version=3, queue='q', at=task.at)
    mod = Modification()
    mod.task_changes.append(tc)
    _fix_versions(mod, task, [], [])
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


def test_worker_lapsed_task_lease_retries_and_says_so():
    """The sets are held until the task arrives, so a worker that ran past its
    own lease fails the claim on the task rather than on any doc. Transient
    like contention, and recorded apart from it: nothing was contended."""
    tc = _doc_dispose(DependencyError(
        "lapsed", depends=[TaskID(id='t1', version=1, queue='q')]))
    assert tc.queue == 'q'
    assert tc.at is not None
    assert 'task lease lapsed' in tc.err
    assert 'doc contention' not in tc.err
    assert tc.attempt == 1


def _selector_raises(error, *, claims=0, max_claims=0):
    """Fail the selector with `error` and return (client, task, work_called)."""
    task = _task(id='t1', version=1, queue='q', claims=claims)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0,
                          max_claims=max_claims, err_queue='err')
    work_called = []
    stop = []

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            work_called.append(t.id)
            return None

        @process.selector
        async def process(t):
            raise error

        stop.append(await worker._process(task, process))

    asyncio.run(run())
    assert work_called == [], "the handler must not run when the selector failed"
    return client, task, stop


def test_worker_selector_move_quarantines_the_task():
    """A sentinel from the selector acts on the task, as one from do_work does.

    The Go worker routes a TakeDocs sentinel through the same machinery; this
    raised out of the worker loop instead, stopping the worker over a task the
    selector had already decided what to do with."""
    client, _, stop = _selector_raises(MoveError("no docs for this one"))
    assert stop == [True], "the worker keeps running"
    tc = client.modify_calls[0].task_changes[0]
    assert tc.queue == 'err'
    assert 'no docs for this one' in tc.err
    assert tc.attempt == 1
    assert tc.reset_claims


def test_worker_selector_retry_requeues_the_task():
    client, _, stop = _selector_raises(RetryError("not yet"))
    assert stop == [True]
    tc = client.modify_calls[0].task_changes[0]
    assert tc.queue == 'q'
    assert tc.at is not None
    assert 'not yet' in tc.err


def test_worker_selector_stop_exits_without_writing():
    """StopWorker leaves the claim to expire, as it does from do_work."""
    client, _, stop = _selector_raises(StopWorker())
    assert stop == [False]
    assert client.modify_calls == []


def test_worker_selector_failure_at_the_claim_limit_is_recorded():
    """A non-sentinel failure on the last allowed claim is written down.

    The next claim would move the task anyway without saying why, so the
    reason is recorded while it is still known. The worker still stops."""
    task = _task(id='t1', version=1, queue='q', claims=3)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0, max_claims=3, err_queue='err')

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            return None

        @process.selector
        async def process(t):
            raise RuntimeError("selector blew up")

        await worker._process(task, process)

    with pytest.raises(RuntimeError, match="selector blew up"):
        asyncio.run(run())

    tc = client.modify_calls[0].task_changes[0]
    assert tc.queue == 'err'
    assert 'claim limit' in tc.err
    assert 'selector blew up' in tc.err
    assert tc.attempt == 1
    assert tc.reset_claims


def test_worker_handler_failure_at_the_claim_limit_is_recorded():
    """The same for the work phase, which is where Go's second check lives."""
    task = _task(id='t1', version=1, queue='q', claims=3)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0, max_claims=3, err_queue='err')

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            raise RuntimeError("handler blew up")

        await worker._process(task, process)

    with pytest.raises(RuntimeError, match="handler blew up"):
        asyncio.run(run())

    tc = client.modify_calls[0].task_changes[0]
    assert tc.queue == 'err'
    assert 'claim limit' in tc.err
    assert 'handler blew up' in tc.err


def test_worker_handler_failure_below_the_claim_limit_records_nothing():
    """A record is a modification, and a modification resets the claim count,
    so recording below the limit would keep the task from ever reaching it."""
    task = _task(id='t1', version=1, queue='q', claims=1)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0, max_claims=3, err_queue='err')

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            raise RuntimeError("handler blew up")

        await worker._process(task, process)

    with pytest.raises(RuntimeError, match="handler blew up"):
        asyncio.run(run())

    assert client.modify_calls == [], "the task waits out its lease"


def test_worker_handler_failure_without_a_claim_limit_records_nothing():
    task = _task(id='t1', version=1, queue='q', claims=1000)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0, err_queue='err')

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            raise RuntimeError("handler blew up")

        await worker._process(task, process)

    with pytest.raises(RuntimeError, match="handler blew up"):
        asyncio.run(run())

    assert client.modify_calls == []


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
    # The lease the claim granted sets the renewal cadence, so a short one is
    # what brings the failing renewal forward; the worker's requested duration
    # has no say in it.
    task = _task(id='t1', version=1, queue='q', lease_s=0.02)
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


def test_worker_claims_every_set_in_one_call():
    """Every named set is claimed together, or none is.

    Claiming them one at a time lets two workers each hold part of what both
    need, which an ordering can only mitigate. One all-or-nothing claim leaves
    nothing to order.
    """
    task = _task(id='t1', version=1)
    doc_a = _doc(namespace='ns', id='da', version=1, key='aaa')
    doc_b = _doc(namespace='ns', id='db', version=1, key='bbb')
    client = FakeClient(tasks=[task], docs=[doc_a, doc_b])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            return Modification(Modification.deleting(t))

        @process.selector
        async def process(task):
            return [DocClaim('ns', 'bbb'), DocClaim('ns', 'aaa')]

        await worker._process(task, process)

    asyncio.run(run())
    assert len(client.claim_set_calls) == 1, client.claim_set_calls
    sets, matched = client.claim_set_calls[0]
    assert [(c.namespace, c.key) for c in sets] == [('ns', 'bbb'), ('ns', 'aaa')]
    assert matched is task, (
        "the sets must be held until the task arrives, so they expire with it")


# ---------------------------------------------------------------------------
# Renewal
# ---------------------------------------------------------------------------

def test_renewing_updates_state_task():
    # A 0.15s lease renews every 0.1s, so 0.25s covers two renewals.
    task = _task(id='t1', version=1, lease_s=0.15)
    client = FakeClient()

    async def run():
        async with _renewing(client, task, ClaimedDocs()) as state:
            await asyncio.sleep(0.25)
        return state.task.version

    assert asyncio.run(run()) >= 3


def test_renewing_dep_error_sets_state_error():
    task = _task(id='t1', version=1, lease_s=0.15)
    client = FakeClient()
    client.modify_raises = DependencyError("lease lost")

    async def run():
        async with _renewing(client, task, ClaimedDocs()) as state:
            await asyncio.sleep(0.2)
        return state.error

    assert isinstance(asyncio.run(run()), DependencyError)


def test_renewing_renews_the_lease_it_was_granted():
    """A renewal asks for at - modified, never for what the claim requested.

    The granted lease is the only number both sides agree on: a service may
    clamp a requested one, and renewing for the request would leave the task
    free between renewals.
    """
    task = _task(id='t1', version=1, lease_s=0.15)
    client = FakeClient()

    async def run():
        async with _renewing(client, task, ClaimedDocs()):
            await asyncio.sleep(0.15)

    asyncio.run(run())
    arrivals = [a for m in client.modify_calls for a in m.task_arrivals]
    assert arrivals, "the renewer must send an arrival, not a change"
    assert all(abs(a.by_s - 0.15) < 1e-9 for a in arrivals), arrivals
    assert not any(m.task_changes for m in client.modify_calls), (
        "a renewal must carry no value, queue, or attempt")


def test_renewing_renews_an_empty_doc_set():
    """A set with no docs still holds a lock, and still has to be renewed.

    The arrival names the set, so there is nothing to iterate and come up
    empty: this is the case a per-doc renewal silently dropped.
    """
    task = _task(id='t1', version=1, lease_s=0.15)
    client = FakeClient()  # no docs at all

    async def run():
        docs = await client.claim_doc_sets([DocClaim('ns', 'empty')],
                                           task_to_match=task)
        assert list(docs) == [], "the set has no members"
        async with _renewing(client, task, docs):
            await asyncio.sleep(0.15)

    asyncio.run(run())
    arrivals = [a for m in client.modify_calls for a in m.doc_arrivals]
    assert arrivals, "an empty set must still be renewed"
    assert all((a.namespace, a.key) == ('ns', 'empty') for a in arrivals)


def test_renewal_reaches_the_commit_through_the_set():
    """What a renewal moves has to reach the committed modification.

    A doc carries its set's version, so renewing the set invalidates every
    version the handler is holding. The members are deliberately left as they
    were claimed -- keeping two copies in step is what went wrong before --
    and _fix_versions imposes the set's current version at commit, which is
    the only place that correction lives.
    """
    task = _task(id='t1', version=1, lease_s=0.15)
    doc = _doc(namespace='ns', id='d1', key='k')
    client = FakeClient(docs=[doc])

    async def run():
        docs = await client.claim_doc_sets([DocClaim('ns', 'k')], task_to_match=task)
        claimed_at = docs[0].version
        async with _renewing(client, task, docs) as state:
            await asyncio.sleep(0.25)  # two renewals at a 0.1s cadence
        mod = Modification(Modification.deleting(state.docs[0]))
        _fix_versions(mod, state.task, state.docs, state.sets)
        return claimed_at, state.sets[0].version, mod.doc_deletes[0].version

    claimed_at, set_version, deleted_at = asyncio.run(run())
    assert set_version > claimed_at, "the renewals moved the set"
    assert deleted_at == set_version, (
        f"commit named v{deleted_at}, set is at v{set_version}")


def test_a_renewal_already_committed_is_not_abandoned():
    """A renewal the server applied must reach the commit that follows it.

    The body ends while the reply is in flight. Dropping it there loses the
    only record of the version the server now holds, and the commit is then
    built on the version before it -- which the server rejects, discarding
    work that succeeded. The window is one reply per renewal, so the chance of
    hitting it grows with how long the handler runs.
    """
    task = _task(id='t1', version=1, queue='q', lease_s=0.12)
    client = ParkedRenewClient(tasks=[task])

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            # Inside the reply's flight, by construction.
            await client.renewal_applied.wait()
            return Modification(Modification.changing(t, value={'done': True}))

        worker = EntroQWorker(client, 'q', claim_duration_s=60.0)
        await worker._process(task, process)

    asyncio.run(run())

    assert client.renewed is not None, "the renewal never landed"
    granted = client.renewed.tasks_changed[0]
    commits = [m for m in client.modify_calls if m.task_changes]
    assert commits, "the body's commit was never sent"
    assert commits[-1].task_changes[0].version == granted.version, (
        "the commit names the version from before the renewal the server "
        "already applied")


def test_a_renewal_that_does_not_report_the_task_stops_the_work():
    """A reply naming no task leaves the caller without the version it holds.

    Carrying on would commit against a version the server has moved past,
    which fails as a dependency error and throws the work away. Better to stop
    where the reason is known. The Go worker's renewed() errors here.
    """
    task = _task(id='t1', version=1, queue='q', lease_s=0.12)
    client = SilentRenewClient(tasks=[task], omit='task')
    committed = []

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            await asyncio.sleep(0.3)
            committed.append(t.id)
            return Modification(Modification.changing(t, value={'done': True}))

        worker = EntroQWorker(client, 'q', claim_duration_s=60.0)
        await worker._process(task, process)

    with pytest.raises(Exception) as caught:
        asyncio.run(run())
    assert not isinstance(caught.value, DependencyError), (
        "a malformed reply is not a lost claim: the worker must not carry on")
    assert client.renewals >= 1
    assert committed == [], "the handler must not run to completion"


def test_a_renewal_that_does_not_report_a_set_stops_the_work():
    """The same for a set: a doc carries its set's version and nothing else."""
    doc = _doc(namespace='ns', id='d1', key='k')
    task = _task(id='t1', version=1, queue='q', lease_s=0.12)
    client = SilentRenewClient(tasks=[task], docs=[doc], omit='sets')
    committed = []

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            await asyncio.sleep(0.3)
            committed.append(t.id)
            return None

        @process.selector
        async def process(t):
            return [DocClaim('ns', 'k')]

        worker = EntroQWorker(client, 'q', claim_duration_s=60.0)
        await worker._process(task, process)

    with pytest.raises(Exception) as caught:
        asyncio.run(run())
    assert not isinstance(caught.value, DependencyError)
    assert committed == [], "the handler must not run to completion"


def test_the_first_renewal_counts_time_already_spent():
    """The lease starts at the claim, not at the body.

    Claiming doc sets is an all-or-nothing claim against other workers, so it
    can take a real part of the lease. Waiting a full interval after it means
    the first renewal can arrive after the task has already come free. Go
    passes time.Since(claimed) in for exactly this.
    """
    doc = _doc(namespace='ns', id='d1', key='k')
    task = _task(id='t1', version=1, queue='q', lease_s=0.3)  # interval 0.2
    client = SlowClaimClient(tasks=[task], docs=[doc], claim_delay_s=0.25)

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            await asyncio.sleep(0.1)
            return None

        @process.selector
        async def process(t):
            return [DocClaim('ns', 'k')]

        worker = EntroQWorker(client, 'q', claim_duration_s=60.0)
        await worker._process(task, process)

    asyncio.run(run())

    assert client.renewed_at, "the lease was never renewed"
    # The claim spent 0.25s of a 0.2s interval, so the renewal was already due
    # when the body started and must not wait another interval for it.
    assert client.renewed_at[0] < 0.1, (
        f"first renewal {client.renewed_at[0]:.3f}s into a body that began "
        "with the interval already spent")


def test_renewing_advances_set_versions():
    """Each renewal names the set at the version the last one returned."""
    task = _task(id='t1', version=1, lease_s=0.15)
    client = FakeClient(docs=[_doc(namespace='ns', id='d1', key='k')])

    async def run():
        docs = await client.claim_doc_sets([DocClaim('ns', 'k')],
                                           task_to_match=task)
        async with _renewing(client, task, docs) as state:
            await asyncio.sleep(0.25)
        return [g.version for g in state.sets]

    final = asyncio.run(run())
    versions = [a.version for m in client.modify_calls for a in m.doc_arrivals]
    assert versions == sorted(set(versions)), (
        "a renewal must not reuse a version the last one moved: %r" % versions)
    assert final == [max(versions) + 1]


# ---------------------------------------------------------------------------
# Releasing claimed sets
# ---------------------------------------------------------------------------

def _released(client) -> list[tuple[str, str]]:
    """Return the sets released (arrival of zero) across every modify made."""
    return [(a.namespace, a.key)
            for m in client.modify_calls for a in m.doc_arrivals if a.by_s <= 0]


def _run_with_sets(client, task, claims, body):
    """Process task with a handler claiming claims and returning body(task, docs)."""
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)

    @EntroQWorker.handler
    async def process(t, docs):
        return body(t, docs)

    @process.selector
    async def process(t):
        return claims

    asyncio.run(worker._process(task, process))
    return worker


def test_commit_releases_the_sets_it_did_not_hold():
    """Claimed sets go back in the committing modification, not afterwards."""
    task = _task(id='t1', version=1, queue='q')
    client = FakeClient(docs=[_doc(namespace='ns', id='d1', key='watched')])

    _run_with_sets(client, task,
                   [DocClaim('ns', 'watched'), DocClaim('ns', 'empty')],
                   lambda t, docs: Modification(Modification.deleting(t)))

    assert sorted(_released(client)) == [('ns', 'empty'), ('ns', 'watched')]
    # One modification, not two: the releases ride with the commit so no window
    # opens where the task is gone and its docs are still held.
    committed = [m for m in client.modify_calls if m.task_deletes]
    assert len(committed) == 1
    assert len(client.modify_calls) == 1, "a second modify means a separate release"


def test_a_body_that_commits_nothing_still_releases():
    """Returning None ends the body, so the sets still go back."""
    task = _task(id='t1', version=1, queue='q')
    client = FakeClient(docs=[_doc(namespace='ns', id='d1', key='held')])

    _run_with_sets(client, task, [DocClaim('ns', 'held')], lambda t, docs: None)

    assert _released(client) == [('ns', 'held')]
    assert not client.modify_calls[0].task_changes, "the task was left alone"
    assert not client.modify_calls[0].task_deletes


def test_a_body_that_leaves_the_task_alone_still_releases():
    """The sets belong to the body, not to the task."""
    task = _task(id='t1', version=1, queue='q')
    doc = _doc(namespace='ns', id='d1', key='written')
    client = FakeClient(docs=[doc])

    _run_with_sets(client, task, [DocClaim('ns', 'written')],
                   lambda t, docs: Modification(
                       Modification.changing(docs[0], content='done')))

    assert _released(client) == [('ns', 'written')]


def test_a_set_held_into_the_future_is_kept():
    """A body naming an arrival of its own decided, so nothing overrides it."""
    task = _task(id='t1', version=1, queue='q')
    doc = _doc(namespace='ns', id='d1', key='kept')
    client = FakeClient(docs=[doc, _doc(namespace='ns', id='d2', key='freed')])
    later = datetime.now(tz=timezone.utc) + timedelta(minutes=1)

    _run_with_sets(client, task, [DocClaim('ns', 'kept'), DocClaim('ns', 'freed')],
                   lambda t, docs: Modification(
                       Modification.deleting(t),
                       Modification.changing(
                           [d for d in docs if d.key == 'kept'][0], at=later)))

    assert _released(client) == [('ns', 'freed')], (
        "the set written with a future arrival must keep it")


def test_releasing_never_names_an_unclaimed_set():
    """The leasehold bounds the release, whatever the modification mentions."""
    task = _task(id='t1', version=1, queue='q')
    client = FakeClient(docs=[_doc(namespace='ns', id='d1', key='held')])

    _run_with_sets(client, task, [DocClaim('ns', 'held')],
                   lambda t, docs: Modification(
                       Modification.deleting(t),
                       # Names a set this worker never claimed.
                       Modification.inserting(DocData(
                           namespace='ns', key='elsewhere', content=1))))

    assert _released(client) == [('ns', 'held')], (
        "releasing moves a version, so it may only name what was claimed here")


# ---------------------------------------------------------------------------
# FatalWorker
# ---------------------------------------------------------------------------

def test_fatal_worker_stops_the_worker_and_leaves_the_task():
    """A fatal handler error stops the loop and disposes of nothing.

    The task is neither retried nor quarantined, so it comes back when its lease
    lapses -- for a worker that may do better. Mirrors Go's FatalError.
    """
    task = _task(id='t1', version=1, queue='q')
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            raise FatalWorker("configuration is unreadable")

        with pytest.raises(FatalWorker, match="unreadable"):
            await worker._process(task, process)

    asyncio.run(run())
    assert client.modify_calls == [], (
        "a fatal error must not retry, quarantine, or otherwise write")


def test_fatal_worker_is_an_ordinary_exception():
    """It derives from Exception, so broad handlers can still catch it.

    Stopping the worker is a decision about the program, not an interruption of
    it, which is why this is not a BaseException like KeyboardInterrupt.
    """
    assert issubclass(FatalWorker, Exception)
    try:
        raise FatalWorker("x")
    except Exception:
        pass
    else:
        raise AssertionError("FatalWorker escaped 'except Exception'")


def test_a_finisher_failure_is_logged_not_fatal():
    """The commit already landed, so a finisher error must not discard it.

    Matches the Go worker, where OnSuccess is optimistic: its error is logged and
    the task stays handled. Python used to let any finisher error kill the worker,
    which was stricter than the reference.
    """
    task = _task(id='t1', version=1, queue='q')
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            return Modification(Modification.deleting(t))

        @process.finisher
        async def process(t, docs):
            raise RuntimeError("the webhook is down")

        return await worker._process(task, process)

    assert asyncio.run(run()) is True, "the worker keeps going"
    assert client.modify_calls[0].task_deletes, "the commit still happened"


def test_a_fatal_finisher_does_stop_the_worker():
    """The one finisher error that is not swallowed."""
    task = _task(id='t1', version=1, queue='q')
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0)

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            return Modification(Modification.deleting(t))

        @process.finisher
        async def process(t, docs):
            raise FatalWorker("unrecoverable after commit")

        with pytest.raises(FatalWorker):
            await worker._process(task, process)

    asyncio.run(run())


# ---------------------------------------------------------------------------
# Dispositions reset the claim count, as Go's RetryOrQuarantine does
# ---------------------------------------------------------------------------

def _disposed(exc, **worker_kwargs):
    """Return the task change a disposition of exc produces."""
    task = _task(id='t1', version=1, queue='q')
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0, **worker_kwargs)
    asyncio.run(worker._dispose(task, exc))
    return client.modify_calls[0].task_changes[0]


def test_a_retry_resets_the_claim_count():
    """A handled failure must not leave its claims counting toward the limit.

    Go's RetryOrQuarantine always resets: the claims before a handled failure do
    not point at a poison pill. Without this, a worker with both max_claims and
    max_attempts set accumulates a claim per retry and is quarantined as
    "maximum claims exceeded" long before it exhausts its attempts.
    """
    change = _disposed(RetryError("transient"))
    assert change.reset_claims is True
    assert change.attempt == 1
    assert change.queue == 'q', "a retry stays in its own queue"


def test_a_quarantine_resets_the_claim_count():
    change = _disposed(MoveError("poison"), err_queue='err')
    assert change.reset_claims is True
    assert change.queue == 'err'
    assert change.at is None, "a quarantined task is for inspection, available now"


def test_the_claim_limit_move_records_an_attempt_and_resets():
    """The claim-limit move is a disposition too, so it looks like one.

    Go routes this through the same Quarantine path, which counts the attempt,
    records the reason, and resets the claims -- so an inspected task re-queued
    by hand starts its budget over instead of tripping the limit immediately.
    """
    task = _task(id='t1', version=1, queue='q', claims=9)
    client = FakeClient(tasks=[task])
    worker = EntroQWorker(client, 'q', claim_duration_s=60.0, max_claims=3,
                          err_queue='err')

    async def run():
        @EntroQWorker.handler
        async def process(t, docs):
            raise AssertionError("the handler must not run past the claim limit")

        return await worker._process(task, process)

    assert asyncio.run(run()) is True
    change = client.modify_calls[0].task_changes[0]
    assert change.queue == 'err'
    assert change.reset_claims is True
    assert change.attempt == 1
    assert 'maximum claims exceeded' in change.err


def test_fix_versions_corrects_arrivals():
    """An arrival names a version too, so renewal has to reach it.

    A handler may defer its task or hold a set past the body with an arrival --
    the documented way to do the latter. Left uncorrected, both are pinned at
    the version they were claimed at, so the first renewal makes the whole
    commit fail and the completed work is thrown away.
    """
    task = _task(id='t1', version=9)
    group = _doc(namespace='ns', id='', key='k', version=4)
    doc = _doc(namespace='ns', id='d1', key='k', version=1)
    mod = Modification(
        Modification.arriving(task, 30.0),
        Modification.arriving(group, 30.0),
    )
    # As a handler would have them: stale, from claim time.
    mod.task_arrivals[0].version = 1
    mod.doc_arrivals[0].version = 1

    _fix_versions(mod, task, [doc], [group])

    assert mod.task_arrivals[0].version == 9, "the task arrival takes the renewed version"
    assert mod.doc_arrivals[0].version == 4, "the set arrival takes the set's version"
