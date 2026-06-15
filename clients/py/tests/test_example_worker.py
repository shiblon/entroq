"""End-to-end test that runs examples/worker/example_worker.py against a real
in-memory EntroQ server — the Python analog of Go's testable examples.

The server is started with ``go run ./cmd/eqmem serve`` (mirroring how the JS
integration tests spawn eqmemsvc). Requires the Go toolchain; skipped if ``go``
is not on PATH.
"""
import asyncio
import importlib.util
import os
import shutil
import signal
import socket
import subprocess
import time
from pathlib import Path

import httpx
import pytest

from entroq.json import EntroQJSON

_REPO_ROOT = Path(__file__).resolve().parents[3]
_EXAMPLE = _REPO_ROOT / "clients" / "py" / "examples" / "worker" / "example_worker.py"


def _load_example():
    spec = importlib.util.spec_from_file_location("example_worker", _EXAMPLE)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture(scope="session")
def eqmem_url():
    """Run an ephemeral in-memory EntroQ server via `go run`; yield its HTTP URL."""
    if shutil.which("go") is None:
        pytest.skip("go toolchain not available")

    http_port, grpc_port = _free_port(), _free_port()
    # `go run` runs the compiled server as a *child*; SIGTERM to the parent
    # leaks that child (verified). start_new_session gives the pair their own
    # process group so teardown can signal the whole group.
    proc = subprocess.Popen(
        ["go", "run", "./cmd/eqmem", "serve",
         "--http_port", str(http_port),
         "--port", str(grpc_port)],
        cwd=_REPO_ROOT,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        start_new_session=True,
    )

    url = f"http://127.0.0.1:{http_port}"
    deadline = time.monotonic() + 60  # `go run` compiles before it serves.
    try:
        while True:
            if proc.poll() is not None:
                out = proc.stdout.read().decode() if proc.stdout else ""
                raise RuntimeError(f"eqmem exited early (code {proc.returncode}):\n{out}")
            try:
                if httpx.get(f"{url}/api/v0/time", timeout=0.5).status_code == 200:
                    break
            except httpx.HTTPError:
                pass
            if time.monotonic() > deadline:
                raise RuntimeError("timed out waiting for eqmem to start")
            time.sleep(0.1)
        yield url
    finally:
        pgid = os.getpgid(proc.pid)
        os.killpg(pgid, signal.SIGTERM)
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(pgid, signal.SIGKILL)


async def _run(url: str) -> list:
    mod = _load_example()
    eq = EntroQJSON(url)
    try:
        return await mod.run_demo(eq, queue="/example/test-queue")
    finally:
        await eq._http.aclose()


def test_example_worker_drains_queue(eqmem_url):
    processed = asyncio.run(asyncio.wait_for(_run(eqmem_url), timeout=30))
    assert sorted(processed) == ["task-1", "task-2", "task-3"]
