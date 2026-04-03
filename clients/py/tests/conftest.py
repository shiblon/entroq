"""pytest fixtures: session-scoped PostgreSQL container, per-test EntroQ client."""

import importlib.resources
import os

import psycopg
import pytest
from testcontainers.postgres import PostgresContainer

from entroq.pg import EntroQ

_SCHEMA_SQL = importlib.resources.files('entroq.pg').joinpath('schema.sql')

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
def eq(pg_connstr):
    """Yield a fresh EntroQ client; truncate all test tables before each test."""
    with psycopg.connect(pg_connstr, autocommit=True) as conn:
        conn.execute('TRUNCATE entroq.tasks')
        conn.execute('TRUNCATE test_counter')
    yield EntroQ(pg_connstr)
