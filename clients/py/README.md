# EntroQ (Python client)

Python client for [EntroQ](https://github.com/shiblon/entroq), a fault-tolerant,
competing-consumer task queue with exactly-once semantics and an atomic
`Modify` operation over tasks and a companion key/value doc store.

This package provides an async HTTP/Connect client and a small worker framework.
Run one of the Go services (`eqpg serve`, `eqmem serve`, `eqredis serve`, or
`eqsqlite serve`) and connect with `EntroQJSON`.

## Install

```sh
pip install entroq
```

## Quick start

A worker claims tasks from one or more queues and dispatches them to a handler:

```python
import asyncio
from entroq import EntroQWorker, Modification
from entroq.json import EntroQJSON

@EntroQWorker.handler
async def process(task, docs):
    # ... do the work ...
    return Modification(Modification.deleting(task))   # ack by deleting

async def main():
    async with EntroQJSON("http://localhost:8080") as eq:
        await EntroQWorker(eq, "my-queue").run(process)

asyncio.run(main())
```

The default HTTP client has no transport deadline. Cancel the calling asyncio
task directly, wrap it in `asyncio.timeout()`, or pass `timeout_s` to `claim()`
when a bounded wait is required. The client exposes its configured
`httpx.AsyncClient` as `eq.http`; pass `http_client=` to supply a customized
caller-owned client and timeout policy. Injected clients are not closed by
`eq.aclose()`.
Transport failures raise `TransportError`, preserving the original exception
and indicating whether the request is known to be safe to retry.

## Documentation

See the [main repository](https://github.com/shiblon/entroq) for the concepts
(tasks, claims, the atomic `Modify`, the doc store, and the `/gc=` naming
convention) and the server/backends the Go side provides.
