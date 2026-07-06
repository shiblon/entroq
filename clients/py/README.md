# EntroQ (Python client)

Python client for [EntroQ](https://github.com/shiblon/entroq), a fault-tolerant,
competing-consumer task queue with exactly-once semantics and an atomic
`Modify` operation over tasks and a companion key/value doc store.

This package provides an async client and a small worker framework. It speaks to
EntroQ two ways:

- **`EntroQJSON`** — talks to a Go EntroQ server over its HTTP/Connect API. Use
  this when a server is already running (`eqpg`/`eqmem`/`eqredis serve`).
- **`EntroQ`** (in `entroq.experimental.pg`) — talks **directly to PostgreSQL**,
  no Go server in the path. It uses the same stored procedures the Go `eqpg`
  backend does, and bundles the canonical schema so a pip-only environment can
  initialize a database without a Go toolchain. Requires the `pg` extra.

  > **Experimental.** Anything under `entroq.experimental` has no stability
  > guarantee and may change or be removed in any release, including patches. The
  > stable, supported path is `EntroQJSON` against a Go server. Pin an exact
  > version if you depend on the direct client.

## Install

```sh
pip install entroq            # client + worker (JSON/HTTP)
pip install "entroq[pg]"      # adds the direct-PostgreSQL backend (psycopg 3)
```

## Quick start

A worker claims tasks from one or more queues and dispatches them to a handler:

```python
import asyncio
from entroq import EntroQWorker, Modification
from entroq.json import EntroQJSON

eq = EntroQJSON("http://localhost:8080")

@EntroQWorker.handler
async def process(task, docs):
    # ... do the work ...
    return Modification(Modification.deleting(task))   # ack by deleting

asyncio.run(EntroQWorker(eq, "my-queue").run(process))
```

Talking straight to PostgreSQL instead of a server (experimental, see above):

```python
from entroq.experimental.pg import EntroQ

eq = EntroQ("host=localhost dbname=entroq user=entroq password=secret")
```

A worker backed by the direct-PostgreSQL client also carries EntroQ's built-in,
always-on garbage collection for `/gc=`-marked queues on the backend's behalf; a
worker talking to a Go server does not, since the server collects for itself.

## Documentation

See the [main repository](https://github.com/shiblon/entroq) for the concepts
(tasks, claims, the atomic `Modify`, the doc store, and the `/gc=` naming
convention) and the server/backends the Go side provides.
