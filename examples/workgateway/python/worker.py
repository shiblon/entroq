#!/usr/bin/env python3
"""A resident EntroQ worker over the work gateway, in stdlib Python.

It spawns `eqlink work` as a child -- the gateway, which runs the EntroQ worker
loop (claim, renew, commit) on our behalf -- and speaks the small
newline-delimited JSON protocol over the child's stdin/stdout. The whole thing is
wrapped in a reconnect loop keyed on the gateway's exit code, so this process
survives a restarting or relocating EntroQ backend without dying.

That is the recipe for a long-lived host -- e.g. a monolith where the worker is
one of many things in the process -- for which crashing on a backend blip would
be a self-inflicted outage. If this worker were instead its own supervised
service, you could drop the reconnect loop entirely: let the process exit when
the gateway does, and let your platform (systemd/k8s/docker) restart it. See
docs/workgateway-protocol.md for both recipes and the full contract.

Prerequisites: a reachable EntroQ gRPC service and `eqlink` on PATH.

    ENTROQ_ADDR=localhost:37706 QUEUE=in python3 worker.py
"""
import json
import os
import signal
import subprocess
import sys
import time

ENTROQ_ADDR = os.environ.get("ENTROQ_ADDR", "localhost:37706")
QUEUE = os.environ.get("QUEUE", "in")

# Gateway exit codes follow sysexits.h. The one bit we branch on is retry-vs-stop:
# 75 (EX_TEMPFAIL) is a transient backend blip to reconnect on; 0 (clean),
# 78 (caller fault), and 70 (gateway fault) are all terminal -- stop and surface.
EXIT_TRANSIENT = 75


def handle(task):
    """Do the work for one task and return the result message.

    `task` is the protojson of an api.Task; its JSON payload is under "value".
    Here we just log it and consume the task -- replace this with real work.
    """
    print(f"[worker] task {task.get('id')}: value={task.get('value')!r}", file=sys.stderr)
    # "ok" commits; "ack" is the shorthand for "I consumed this task", so the
    # gateway deletes it for us. To produce new work instead, add a "modification"
    # (a protojson ModifyRequest); leave its claimant_id empty.
    return {"type": "result", "outcome": "ok", "ack": True}


def serve(proc):
    """Answer phase messages from one gateway child until it exits.

    Strict request/response: the gateway sends a phase message, we reply. An
    out-of-band {"type":"error"} may arrive in place of a phase message -- it is
    one-way (we only log it); the reconnect decision comes from the exit code.
    """
    for line in proc.stdout:
        msg = json.loads(line)
        kind = msg.get("type")
        if kind == "doWork":
            reply = handle(msg["task"])
            proc.stdin.write((json.dumps(reply) + "\n").encode())
            proc.stdin.flush()
        elif kind == "error":
            print(
                f"[worker] gateway error [{msg.get('class')}]: {msg.get('message')}",
                file=sys.stderr,
            )
        else:
            print(f"[worker] unexpected message {kind!r}", file=sys.stderr)


def run_gateway():
    """Spawn one gateway child, serve it, and return its exit code.

    We register only the work phase (--work). stderr is inherited (the default),
    so the gateway's diagnostics reach ours; do not redirect it away or you lose
    them.
    """
    proc = subprocess.Popen(
        ["eqlink", "--entroq", ENTROQ_ADDR, "work", "--queue", QUEUE, "--work"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
    )
    _set_child(proc)
    try:
        serve(proc)
    finally:
        proc.stdin.close()
        proc.wait()
    _set_child(None)
    return proc.returncode


def main():
    backoff = 0.2
    while True:
        started = time.monotonic()
        code = run_gateway()
        if code == EXIT_TRANSIENT:
            # A run that lasted a while before the blip is a fresh outage, not a
            # continuing one, so reset the backoff.
            if time.monotonic() - started > 30:
                backoff = 0.2
            print(
                f"[worker] backend unavailable (exit {code}); reconnecting in {backoff:.1f}s",
                file=sys.stderr,
            )
            time.sleep(backoff)
            backoff = min(backoff * 2, 5.0)
            continue
        # Clean stop or a caller/gateway fault: terminal, do not retry.
        print(f"[worker] gateway stopped (exit {code}); exiting", file=sys.stderr)
        sys.exit(code)


# Graceful shutdown: pass SIGINT/SIGTERM to the gateway so it winds down the
# current claim cleanly instead of orphaning it until the lease expires.
_child = None


def _set_child(proc):
    global _child
    _child = proc


def _shutdown(signum, frame):
    if _child is not None and _child.poll() is None:
        _child.terminate()
    sys.exit(0)


if __name__ == "__main__":
    signal.signal(signal.SIGINT, _shutdown)
    signal.signal(signal.SIGTERM, _shutdown)
    main()
