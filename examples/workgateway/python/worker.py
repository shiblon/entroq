#!/usr/bin/env python3
"""An EntroQ worker in Python, through the work gateway.

Prerequisites: a reachable EntroQ and `eqlink` on PATH.

    ENTROQ_ADDR=localhost:37706 QUEUE=in python3 worker.py

WORKERS runs that many at once (default 2), each holding one task at a time.
EQLINK_ARGS adds flags for `eqlink work`, such as "--lease 10s".
"""
import os
import shlex
import sys

from eqgateway import Gateway, Worker, M


def work(w, ctx):
    """Process one task and say what becomes of it.

    w.task is the protojson of an api.Task, with its JSON value in "value".
    Return a Modification to commit (M(M.deleting(w.task)) once it is
    consumed), or None to commit nothing; raise RetryError or MoveError to give
    up on it. Long work can watch ctx.stopping, or use ctx.sleep, to finish
    early when the worker drains.
    """
    print(f"[worker] task {w.task['id']}: value={w.task['value']!r}", file=sys.stderr)
    return M(M.deleting(w.task))


if __name__ == "__main__":
    queue = os.environ.get("QUEUE", "in")
    gateway = Gateway(
        eqlink=os.environ.get("EQLINK", "eqlink"),
        entroq=os.environ.get("ENTROQ_ADDR", "localhost:37706"),
        args=shlex.split(os.environ.get("EQLINK_ARGS", "")),
    )
    try:
        gateway.run(*(Worker([queue], work) for _ in range(int(os.environ.get("WORKERS", "2")))))
    except Exception as e:
        sys.exit(f"[worker] {e}")
