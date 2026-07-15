# Work gateway examples

Minimal clients for the EntroQ work gateway (`eqlink work`). A gateway client is
a worker written in any language: it never imports EntroQ or gRPC, it just speaks
a small newline-delimited JSON protocol while the gateway runs the hard part
(claim, renew, commit) in Go. The full contract and the client recipes are in
[../../docs/workgateway-protocol.md](../../docs/workgateway-protocol.md).

## `python/worker.py` — a resident worker with reconnect

A long-lived worker that spawns `eqlink work` as a child and survives a
restarting or relocating EntroQ backend by reconnecting, keyed on the gateway's
exit code (`75` = transient → reconnect; anything else → stop). This is the
recipe for a **monolith** or any process that must not die when the backend
blips.

The simpler **crash-only** recipe — for a worker that is its own supervised
service — is to drop the reconnect loop and let the process exit when the gateway
does, leaving the restart to your platform. Both are covered in the protocol doc.

Run it against a reachable EntroQ (the default `--entroq` address is
`localhost:37706`):

```sh
ENTROQ_ADDR=localhost:37706 QUEUE=in python3 python/worker.py
```

Then insert a task on that queue (e.g. with `eqc` or another producer) and watch
the worker claim and consume it. Stop it with Ctrl-C; it signals the gateway to
wind down the current claim cleanly.

It is stdlib-only and deliberately tiny — the point is the shape of a correct
client, not a framework.
