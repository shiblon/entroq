# Work gateway example

An EntroQ worker in Python through the work gateway (`eqlink work`), stdlib
only. A gateway client never imports EntroQ or gRPC: it speaks a small JSON
protocol while the gateway runs the worker loop (claim, renew, commit) in Go.

- **`python/worker.py`** is the worker: one `work(w, ctx)` function that
  says what becomes of each task.
- **`python/pipeline.py`** runs several workers in one process, all sharing one
  gateway child: two move tasks along to the next queue, one consumes them and
  counts them in a doc, and one shows retry, quarantine and long work.
- **`python/eqgateway.py`** is the client library underneath, and the thing to
  read if you are writing one in another language. It is the whole client side
  of the protocol: one blocking `exchange`, a switch on its answer, the stop
  rule, and a keepalive around long work.

## Running it

Needs a reachable EntroQ and `eqlink` on `PATH`. The worker starts its own
gateway as a child process, so nothing else need be running:

```sh
ENTROQ_ADDR=localhost:37706 QUEUE=in python3 python/worker.py
```

By default it runs one `eqlink work --addr 127.0.0.1:0` child and two workers
over HTTP to it (`WORKERS` sets how many), each a session holding one task at a
time. `TRANSPORT=stdio` runs a child per worker and talks to it over stdin and
stdout instead. Either way the gateway
dies with the worker, however the worker dies.

`pipeline.py` runs the same way, on queues under `demo/`
(`ENTROQ_ADDR=localhost:37706 python3 python/pipeline.py`).

Insert a task on the queue (e.g. with `eqc`) and watch it go. The first Ctrl-C
drains: work in hand finishes and commits, nothing new is claimed. A second
Ctrl-C hangs up at once and leaves any task in hand to its lease.

The worker passes each signal on to its gateways, which do the draining, and
answers until each says quit. A gateway runs in a session of its own so that a
signal reaches it once, through the worker; under a supervisor, signal the
worker alone (systemd `KillMode=mixed`).
