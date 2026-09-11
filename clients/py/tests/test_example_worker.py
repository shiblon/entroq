"""End-to-end test that runs examples/worker/example_worker.py against a real
in-memory EntroQ server — the Python analog of Go's testable examples.

The server comes from the ``eqmem_url`` fixture in conftest.py.
"""
import asyncio
import importlib.util
from pathlib import Path

from entroq.json import EntroQJSON
from entroq.types import Modification, TaskData

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
