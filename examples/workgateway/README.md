# Work gateway examples

Minimal clients for the EntroQ work gateway (`eqlink work`). A gateway client is
a worker written in any language: it never imports EntroQ or gRPC, it just speaks
a small newline-delimited JSON protocol while the gateway runs the hard part
(claim, renew, commit) in Go. The full contract and the client recipes are in
[../../docs/workgateway-protocol.md](../../docs/workgateway-protocol.md).

These Python examples keep the worker's actual work in one shared file and show
the same logic over **both transports**:

- **`handler.py`** — the business logic (`handle(task)`), shared by both.
- **`pipe_worker.py`** — the **pipe** transport: spawns `eqlink work` as a child
  and talks to it over stdio. stdlib only.
- **`ws_worker.py`** — the **WebSocket** transport: dials an already-running
  `eqlink work --addr` gateway. Needs `pip install websockets`.

Both are *resident* workers with a reconnect loop — the recipe for a long-lived
process (a monolith) that must not die when the backend blips. Each keys its
reconnect decision on the signal the gateway gives when it stops: the pipe worker
on the process **exit code** (`75` = transient → reconnect), the WebSocket worker
on the **close code** (`1013` = transient → reconnect). Anything else stops. The
simpler **crash-only** recipe — for a worker that is its own supervised service —
drops the loop entirely and lets the platform restart the process; see the
protocol doc.

## Pipe

Needs a reachable EntroQ and `eqlink` on `PATH`. The worker spawns `eqlink work`
itself, so nothing else need be running:

```sh
ENTROQ_ADDR=localhost:37706 QUEUE=in python3 pipe_worker.py
```

## WebSocket

Run a gateway as a service first, then dial it:

```sh
# terminal 1: the gateway, serving WebSocket
eqlink --entroq localhost:37706 work --addr :8080 --queue in --work

# terminal 2: the worker
GATEWAY_WS_URL=ws://localhost:8080/work?queue=in&work=1 python3 ws_worker.py
```

## Trying it

Insert a task on the queue (e.g. with `eqc`) and watch the worker claim and
consume it. Stop with Ctrl-C — it closes cleanly, letting the gateway wind down
the current claim rather than orphaning it for a lease period.

They are deliberately tiny: the point is the shape of a correct client, not a
framework.
