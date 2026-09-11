"""pytest fixtures: session-scoped PostgreSQL container and EntroQ server, and
a per-test EntroQ client."""

import importlib.resources
import os
import shutil
import signal
import socket
import subprocess
import time
from pathlib import Path

import httpx
import psycopg
import pytest
from testcontainers.postgres import PostgresContainer

from entroq.experimental.pg import EntroQ

_REPO_ROOT = Path(__file__).resolve().parents[3]

_SCHEMA_SQL = importlib.resources.files('entroq.experimental.pg').joinpath('schema.sql')

_PG_IMAGE    = 'postgres:11'
_PG_PASSWORD = 'password'
_PG_USER     = 'postgres'
_PG_DB       = 'postgres'

# Docker Desktop on macOS uses a non-default socket path. Set DOCKER_HOST
# before testcontainers initializes if it's not already set.
if 'DOCKER_HOST' not in os.environ:
    for _sock in (
        '/var/run/docker.sock',
        os.path.expanduser('~/.docker/run/docker.sock'),
    ):
        if os.path.exists(_sock):
            os.environ['DOCKER_HOST'] = f'unix://{_sock}'
            break


@pytest.fixture(scope='session')
def pg_connstr():
    """Start a throwaway PostgreSQL container and yield a connection string.

    testcontainers handles the run/readiness/stop lifecycle. schema.sql is
    applied once after the container is ready.
    """
    with PostgresContainer(
        image=_PG_IMAGE,
        username=_PG_USER,
        password=_PG_PASSWORD,
        dbname=_PG_DB,
    ) as pg:
        connstr = (
            f'host={pg.get_container_host_ip()}'
            f' port={pg.get_exposed_port(5432)}'
            f' dbname={_PG_DB} user={_PG_USER} password={_PG_PASSWORD}'
        )
        # Apply schema once for the session. autocommit=True because some DDL
        # statements (e.g. CREATE EXTENSION) run better outside a transaction.
        with psycopg.connect(connstr, autocommit=True) as conn:
            conn.execute(_SCHEMA_SQL.read_text())
            # Side table used by transaction-atomicity tests.
            conn.execute('''
                CREATE TABLE IF NOT EXISTS test_counter (
                    name  TEXT PRIMARY KEY,
                    count INT  NOT NULL DEFAULT 0
                )
            ''')
        yield connstr


@pytest.fixture
async def eq(pg_connstr):
    """Yield a fresh EntroQ client; truncate all test tables before each test."""
    with psycopg.connect(pg_connstr, autocommit=True) as conn:
        conn.execute('TRUNCATE entroq.tasks')
        conn.execute('TRUNCATE entroq.docs')
        conn.execute('TRUNCATE test_counter')
    client = EntroQ(pg_connstr)
    try:
        yield client
    finally:
        await client.aclose()


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
