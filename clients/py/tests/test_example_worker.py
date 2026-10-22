"""End-to-end test that runs examples/worker/example_worker.py against a real
in-memory EntroQ server — the Python analog of Go's testable examples.

The server comes from the ``eqmem_url`` fixture in conftest.py.
"""
import asyncio
import importlib.util
import uuid
from pathlib import Path

import pytest

from entroq.json import EntroQJSON
from entroq.types import (
    DependencyError, DocClaim, DocData, InvalidArgumentError, Modification,
    TaskData,
)
from entroq.worker import EntroQWorker

_REPO_ROOT = Path(__file__).resolve().parents[3]
_EXAMPLE = _REPO_ROOT / "clients" / "py" / "examples" / "worker" / "example_worker.py"


def _load_example():
    spec = importlib.util.spec_from_file_location("example_worker", _EXAMPLE)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


async def _run(url: str) -> list:
    mod = _load_example()
    async with EntroQJSON(url) as eq:
        return await mod.run_demo(eq, queue="/example/test-queue")


def test_example_worker_drains_queue(eqmem_url):
    processed = asyncio.run(asyncio.wait_for(_run(eqmem_url), timeout=30))
    assert sorted(processed) == ["task-1", "task-2", "task-3"]


async def test_task_move_releases_claim_over_json(eqmem_url):
    queue = "/test/python-json/move-release"
    error_queue = queue + "/error"
    async with EntroQJSON(eqmem_url) as eq:
        inserted = await eq.modify(Modification(
            Modification.inserting(TaskData(queue=queue, value="poison")),
        ))
        claimed = await eq.try_claim(queue, duration_ms=60_000)
        assert claimed is not None
        assert claimed.id == inserted.tasks_inserted[0].id

        before = await eq.time()
        moved = await eq.modify(Modification(
            Modification.changing(claimed, queue=error_queue, err="quarantined"),
        ))
        after = await eq.time()

        changed = moved.tasks_changed[0]
        assert changed.queue == error_queue
        assert changed.claimant == ""
        assert before <= changed.at <= after

        reclaimed = await eq.try_claim(error_queue, duration_ms=1_000)
        assert reclaimed is not None
        assert reclaimed.id == claimed.id


async def _claim_after_old_httpx_timeout(url: str):
    queue = "/test/python-json/long-poll"
    async with EntroQJSON(url) as consumer, EntroQJSON(url) as producer:
        claim = asyncio.create_task(consumer.claim(queue))
        await asyncio.sleep(5.25)
        assert not claim.done(), "claim inherited httpx's former five-second read timeout"
        await producer.modify(Modification(Modification.inserting(TaskData(queue=queue, value="ready"))))
        return await asyncio.wait_for(claim, timeout=3)


def test_json_claim_stays_open_past_httpx_default(eqmem_url):
    task = asyncio.run(_claim_after_old_httpx_timeout(eqmem_url))
    assert task.value == "ready"


async def test_json_docs_filters_against_live_server(eqmem_url):
    """Exercise the nested DocQuery field paths against real transcoding.

    Recovered from a 2026-09-11 stash. Client-side field-path construction can
    fail silently -- a wrong or missing nested name yields an empty filter
    rather than an error -- so these paths are only worth testing against a
    real server.
    """
    ns = f"live-{uuid.uuid4().hex}"
    async with EntroQJSON(eqmem_url) as eq:
        res = await eq.modify(Modification(*[
            Modification.inserting(DocData(
                namespace=ns, key=key, secondary_key=sub, content={'k': key},
            ))
            for key, sub in (('a', '1'), ('b', '1'), ('b', '2'), ('c', '1'))
        ]))
        assert len(res.docs_inserted) == 4

        # The probe for arrival: a query naming no namespace is refused, which
        # proves the request was read rather than silently emptied by a wrong
        # field path. See test_docs_no_filter_reaches_the_server.
        with pytest.raises(InvalidArgumentError):
            await eq.docs()
        listed = await eq.docs(namespace=ns)
        exact = await eq.docs(namespace=ns, key_exact='b')
        by_id = await eq.docs(namespace=ns, ids=[listed[0].id, listed[3].id])

        assert [(d.key, d.secondary_key) for d in listed] == [
            ('a', '1'), ('b', '1'), ('b', '2'), ('c', '1'),
        ]
        assert [(d.key, d.secondary_key) for d in exact] == [
            ('b', '1'), ('b', '2'),
        ]
        assert [d.id for d in by_id] == [listed[0].id, listed[3].id]


async def test_worker_claims_and_releases_doc_sets_over_json(eqmem_url):
    """A worker's whole doc round trip against a real server.

    The pieces only meet here: the handler's DocClaim list becomes one atomic
    claim naming the task to expire with, the commit resets the task's claim
    count, and a set the commit did not write is released once the task is
    gone. A wrong nested name in any of those requests claims or releases
    nothing rather than failing.

    The renewal cadence is not exercised: the service clamps a claim up to its
    lease floor, so the shortest lease a client can get here is long enough
    that waiting out two thirds of it would dominate the suite.
    """
    ns = f"live-{uuid.uuid4().hex}"
    queue = f"{ns}/q"
    async with EntroQJSON(eqmem_url) as eq:
        await eq.modify(Modification(
            Modification.inserting(TaskData(queue=queue, value="work")),
            Modification.inserting(DocData(namespace=ns, key="cfg", content={"n": 1})),
        ))

        seen = []

        @EntroQWorker.handler
        async def process(task, docs):
            seen.append([(d.key, d.content) for d in docs])
            # The set is held while the handler runs: a claim of it now gets
            # nothing back, because another claimant cannot have it.
            with pytest.raises(DependencyError):
                async with EntroQJSON(eqmem_url) as other:
                    await other.claim_doc_sets(
                        [DocClaim(ns, "cfg")], duration_ms=1000)
            return Modification(Modification.deleting(task))

        @process.selector
        async def process(task):
            return [DocClaim(ns, "cfg"), DocClaim(ns, "empty")]

        worker = EntroQWorker(eq, queue)
        task = await eq.try_claim(queue)
        assert task is not None

        await asyncio.wait_for(worker._process(task, process), timeout=30)

        assert seen == [[("cfg", {"n": 1})]], "the handler saw the set's members"
        assert await eq.tasks(queue=queue) == [], "the task was committed"

        # The commit deleted the task and wrote neither set, so both were
        # released on the way out and are free to claim again.
        again = await eq.claim_doc_sets(
            [DocClaim(ns, "cfg"), DocClaim(ns, "empty")], duration_ms=1000)
        assert [g.key for g in again.sets] == ["cfg", "empty"]
