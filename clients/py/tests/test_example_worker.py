"""End-to-end test that runs examples/worker/example_worker.py against a real
in-memory EntroQ server — the Python analog of Go's testable examples.

The server comes from the ``eqmem_url`` fixture in conftest.py.
"""
import asyncio
import importlib.util
from pathlib import Path

from entroq.json import EntroQJSON

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
