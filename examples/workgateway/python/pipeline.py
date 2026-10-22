#!/usr/bin/env python3
"""Several workers in one process, sharing one gateway child.

    words --shout--> shouted --tell--> (gone), counted in a doc
    slow  --nap----> (gone), or slow/dead after two bad attempts

shout uppercases a word and moves its task on to the next queue; two of them
run. tell prints the word, consumes the task and counts it in a doc, all in one
commit, and says so once the commit lands. nap sleeps as many seconds as its
task says, which is long enough to see keepalives go out under a short lease; a
value that is not a number is retried, and quarantined to slow/dead when the
attempts run out. A nap cut short by a drain is retried.

Prerequisites: a reachable EntroQ and `eqlink` on PATH.

    ENTROQ_ADDR=localhost:37706 python3 pipeline.py

The four workers share one `eqlink work` child. EQLINK_ARGS adds flags for
`eqlink work`, such as "--lease 3s".
"""
import os
import shlex
import sys

from eqgateway import DocClaim, Gateway, M, RetryError, Worker

PREFIX = os.environ.get("PREFIX", "demo")


def log(msg):
    print(f"[pipeline] {msg}", file=sys.stderr)


def shout(w, ctx):
    return M(M.changing(w.task, queue=f"{PREFIX}/shouted", value=str(w.value).upper()))


def tell_take(w, ctx):
    return [DocClaim(PREFIX, "told")]


def tell(w, ctx):
    log(f"tell: {w.value}")
    if not w.docs:
        count = M.inserting(namespace=PREFIX, key="told", content=1)
    else:
        doc = w.docs[0]
        count = M.changing(doc, content=doc["content"] + 1)
    return M(M.deleting(w.task), count)


def told(w, ctx):
    log(f"told: {w.task['id']} is gone")


def nap(w, ctx):
    if not isinstance(w.value, (int, float)):
        raise RetryError(f"cannot nap for {w.value!r}", or_move_to=f"{PREFIX}/slow/dead")
    log(f"nap: {w.value}s")
    if not ctx.sleep(w.value):
        raise RetryError("woken early to drain")
    return M(M.deleting(w.task))


if __name__ == "__main__":
    gateway = Gateway(
        eqlink=os.environ.get("EQLINK", "eqlink"),
        entroq=os.environ.get("ENTROQ_ADDR", "localhost:37706"),
        args=shlex.split(os.environ.get("EQLINK_ARGS", "")),
    )
    try:
        gateway.run(
            Worker([f"{PREFIX}/words"], shout),
            Worker([f"{PREFIX}/words"], shout),
            Worker([f"{PREFIX}/shouted"], tell, take=tell_take, on_success=told),
            Worker([f"{PREFIX}/slow"], nap, max_attempts=2, retry_delay_s=1),
        )
    except Exception as e:
        sys.exit(f"[pipeline] {e}")
