"""pytest fixtures for an ephemeral in-memory EntroQ server."""

import os
import shutil
import signal
import socket
import subprocess
import time
from pathlib import Path

import httpx
import pytest

_REPO_ROOT = Path(__file__).resolve().parents[3]


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


@pytest.fixture(scope='session')
def eqmem_url():
    """Run an ephemeral in-memory EntroQ server via `go run`; yield its HTTP URL.

    Tests using this fixture exercise the real REST transcoding, which is the
    only place client-side query parameter names are validated.
    Requires the Go toolchain; skipped if ``go`` is not on PATH.
    """
    if shutil.which('go') is None:
        pytest.skip('go toolchain not available')

    http_port, grpc_port = _free_port(), _free_port()
    # `go run` runs the compiled server as a *child*; SIGTERM to the parent
    # leaks that child (verified). start_new_session gives the pair their own
    # process group so teardown can signal the whole group.
    proc = subprocess.Popen(
        ['go', 'run', './cmd/eqmem', 'serve',
         '--http_port', str(http_port),
         '--port', str(grpc_port)],
        cwd=_REPO_ROOT,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        start_new_session=True,
    )

    url = f'http://127.0.0.1:{http_port}'
    deadline = time.monotonic() + 60  # `go run` compiles before it serves.
    try:
        while True:
            if proc.poll() is not None:
                out = proc.stdout.read().decode() if proc.stdout else ''
                raise RuntimeError(f'eqmem exited early (code {proc.returncode}):\n{out}')
            try:
                if httpx.get(f'{url}/api/v0/time', timeout=0.5).status_code == 200:
                    break
            except httpx.HTTPError:
                pass
            if time.monotonic() > deadline:
                raise RuntimeError('timed out waiting for eqmem to start')
            time.sleep(0.1)
        yield url
    finally:
        pgid = os.getpgid(proc.pid)
        os.killpg(pgid, signal.SIGTERM)
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(pgid, signal.SIGKILL)
