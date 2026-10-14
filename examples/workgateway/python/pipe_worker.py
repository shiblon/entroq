#!/usr/bin/env python3
"""A resident EntroQ worker over the work gateway's pipe transport, stdlib only.

The work itself lives in handler.py; this file is the transport and its
supervision loop.

It spawns `eqlink work` as a child -- the gateway, which runs the EntroQ worker
loop (claim, renew, commit) on our behalf -- and speaks newline-delimited JSON
over the child's stdin and stdout. The client drives: it registers with a
config, says it is ready, and then answers each task the gateway hands it.

The reconnect loop is keyed on the gateway's exit code, so this process
survives a restarting backend. That is the recipe for a long-lived host, such
as a monolith where the worker is one of many things in the process. A worker
that is its own supervised service can drop the loop: exit when the gateway
does, and let the platform restart it.

Prerequisites: a reachable EntroQ gRPC service and `eqlink` on PATH.

    ENTROQ_ADDR=localhost:37706 QUEUE=in python3 pipe_worker.py
"""
import json
import os
import signal
import subprocess
import sys
import time

from handler import handle

ENTROQ_ADDR = os.environ.get("ENTROQ_ADDR", "localhost:37706")
QUEUE = os.environ.get("QUEUE", "in")
EQLINK = os.environ.get("EQLINK", "eqlink")

# The gateway protocols this worker speaks. The gateway picks the highest one
# both sides know, or refuses the config naming what it does speak.
PROTOCOLS = [1]

# Gateway exit codes follow sysexits.h. The one bit worth branching on is
# retry-vs-stop: 75 (EX_TEMPFAIL) is a transient backend blip to reconnect on;
# 0 (clean), 78 (caller fault) and 70 (gateway fault) are terminal.
EXIT_TRANSIENT = 75


class Session:
    """One gateway child and the one conversation it carries."""

    def __init__(self):
        self.proc = subprocess.Popen(
            [EQLINK, "--entroq", ENTROQ_ADDR, "work"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            # stderr is inherited, so the gateway's diagnostics reach ours.
        )
        self.id = ""
        self.in_hand = False
        self.stopping = False

    def send(self, msg):
        msg["session"] = self.id
        self.proc.stdin.write((json.dumps(msg) + "\n").encode())
        self.proc.stdin.flush()

    def recv(self):
        line = self.proc.stdout.readline()
        return json.loads(line) if line else None

    def stop(self):
        """Ask to stop. A task in hand finishes first, because the quit rides
        on its reply; with nothing in hand there is nothing to finish, and
        hanging up is the whole goodbye."""
        self.stopping = True
        if not self.in_hand:
            self.hang_up()

    def hang_up(self):
        if not self.proc.stdin.closed:
            self.proc.stdin.close()

    def serve(self):
        """Run the conversation until the gateway ends it, and return its
        exit code."""
        try:
            self.converse()
        except BrokenPipeError:
            # The gateway went away mid-write; its exit code says why.
            pass
        finally:
            self.hang_up()
            self.proc.wait()
        return self.proc.returncode

    def converse(self):
        self.send({"type": "config", "config": {
            "queues": [QUEUE],
            "protocol_versions": PROTOCOLS,
        }})
        check = self.recv()
        if check is None:
            return
        if check.get("type") != "check":
            report(check)
            return
        self.id = check["session"]
        print(f"[worker] session {self.id} as {check.get('claimant')}, "
              f"work timeout {check.get('work_timeout_s', 0)}s", file=sys.stderr)

        if self.stopping:
            return
        self.send({"type": "ready"})
        while True:
            msg = self.recv()
            if msg is None:
                return
            kind = msg.get("type")
            if kind == "work":
                self.in_hand = True
                reply = handle(msg["task"])
                if self.stopping:
                    reply["quit"] = True
                self.send(reply)
                self.in_hand = False
            elif kind == "quit":
                # The answer to a quit: the last reply committed.
                return
            else:
                report(msg)


def report(msg):
    if msg.get("type") == "error":
        print(f"[worker] gateway error [{msg.get('class')}]: {msg.get('message')}",
              file=sys.stderr)
    else:
        print(f"[worker] unexpected message {msg.get('type')!r}", file=sys.stderr)


_session = None


def _on_signal(signum, frame):
    global _stop_requested
    _stop_requested = True
    if _session is not None:
        _session.stop()


_stop_requested = False


def main():
    global _session
    backoff = 0.2
    while not _stop_requested:
        started = time.monotonic()
        _session = Session()
        code = _session.serve()
        _session = None
        if code == EXIT_TRANSIENT and not _stop_requested:
            # A run that lasted a while before the blip is a fresh outage, not
            # a continuing one, so the backoff starts over.
            if time.monotonic() - started > 30:
                backoff = 0.2
            print(f"[worker] backend unavailable (exit {code}); "
                  f"reconnecting in {backoff:.1f}s", file=sys.stderr)
            time.sleep(backoff)
            backoff = min(backoff * 2, 5.0)
            continue
        print(f"[worker] gateway stopped (exit {code}); exiting", file=sys.stderr)
        sys.exit(code)


if __name__ == "__main__":
    signal.signal(signal.SIGINT, _on_signal)
    signal.signal(signal.SIGTERM, _on_signal)
    main()
