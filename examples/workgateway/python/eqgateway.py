"""A small EntroQ work gateway client, stdlib only.

A worker built on it supplies functions and nothing else:

    def work(w, ctx):
        return M(M.deleting(w.task))

    Worker(["in"], work).run()

work(w, ctx) settles one task: w carries the task, its value and its docs, and
what it returns is committed. Raising RetryError, MoveError or FatalWorker gives
the task another fate. Three more functions are optional: take(w, ctx) names
the doc sets a task needs, on_success(w, ctx) runs after a commit lands, and
on_dependency(w, deps, ctx) picks a fate when a commit loses a dependency race.

A worker is one session, holding at most one task at a time. For more at once,
or for several kinds of work in one process, hand them all to one Gateway:

    Gateway().run(Worker(["a"], work_a), *(Worker(["b"], work_b) for _ in range(4)))

Underneath is the whole client side of the protocol: one blocking call,
exchange, that sends a message and returns the gateway's answer, and a switch on
what comes back. It runs over HTTP to one `eqlink work --addr` child shared by
every session, or over stdio to a child per session.
"""
import http.client
import json
import os
import signal
import socket
import subprocess
import sys
import threading

# The gateway protocols this client speaks.
PROTOCOLS = [1]


class GatewayError(Exception):
    """The gateway ended the session. cls says whether a new one could help:
    "transient" is worth retrying; "caller" and "gateway" are not."""

    def __init__(self, cls, message):
        super().__init__(f"[{cls}] {message}")
        self.cls = cls


class SessionGone(Exception):
    """The gateway no longer has this session, so start a new one."""


class Work:
    """A task in hand: the task (the protojson of an api.Task), its value, the
    docs claimed for it, and the sets they were claimed as. docs and sets are
    empty in take, which decides them."""

    def __init__(self, task, doc_sets=()):
        self.task = task
        self.value = task.get("value")
        self.sets = [s["set"] for s in doc_sets]
        self.docs = [d for s in doc_sets for d in s.get("docs", [])]


class RetryError(Exception):
    """Raise to try the task again later, counting the attempt. Once the
    attempts run out it moves to or_move_to, or to the worker's error queue."""

    def __init__(self, message="", or_move_to=""):
        super().__init__(message)
        self.or_move_to = or_move_to


class MoveError(Exception):
    """Raise to move the task now, to queue or to the worker's error queue."""

    def __init__(self, message="", queue=""):
        super().__init__(message)
        self.queue = queue


class FatalWorker(Exception):
    """Raise to stop every worker with this error, for something no retry can
    fix in this process. The task comes back when its lease lapses."""


class DocClaim(dict):
    """A doc set for take to claim."""

    def __init__(self, namespace, key):
        super().__init__(set={"namespace": namespace, "key": key})


def task_id(task):
    return {"queue": task["queue"], "id": task["id"], "version": task.get("version", 0)}


def doc_id(doc):
    return {"namespace": doc["namespace"], "id": doc["id"], "version": doc.get("version", 0)}


def _is_doc(item):
    return "namespace" in item


def _arriving(data, by_s):
    if by_s:
        data["by_ms"] = int(by_s * 1000)
    return data


class Modification:
    """Task and doc operations committed together, or not at all:

        M(M.deleting(w.task), M.inserting(queue="next", value=v))

    Name a task or doc as it was handed over. The gateway moves the versions
    of the task in hand and its claimed docs to wherever renewal left them."""

    def __init__(self, *ops):
        self.request = {}
        for field, item in ops:
            self.request.setdefault(field, []).append(item)

    @staticmethod
    def inserting(queue="", value=None, by_s=0, **doc):
        """A task, given queue, value, and by_s to delay it; or a doc, given
        namespace, key, and optionally secondary_key and content."""
        if queue:
            return "inserts", _arriving({"queue": queue, "value": value}, by_s)
        return "doc_inserts", doc

    @staticmethod
    def changing(item, by_s=0, **fields):
        """A task or doc with the fields named replaced: a task's queue or
        value, which by_s may delay; a doc's content or keys."""
        if _is_doc(item):
            data = {k: item[k] for k in ("namespace", "id", "key", "secondary_key", "content") if k in item}
            return "doc_changes", {"old_id": doc_id(item), "new_data": {**data, **fields}}
        data = {"queue": item["queue"], "value": item.get("value"), **fields}
        return "changes", {"old_id": task_id(item), "new_data": _arriving(data, by_s)}

    @staticmethod
    def deleting(item):
        return ("doc_deletes", doc_id(item)) if _is_doc(item) else ("deletes", task_id(item))

    @staticmethod
    def depending(item):
        """Commit only if item is still at its version, changing nothing."""
        return ("doc_depends", doc_id(item)) if _is_doc(item) else ("depends", task_id(item))


M = Modification


# Transports. Each has exchange(msg) -> reply, abort() to give up on a waiting
# exchange from another thread, and close().

class HTTPChild:
    """One `eqlink work --addr` child, shared by every session in this process.
    It exits on its own when this process does."""

    def __init__(self, argv):
        self.argv = argv + ["--addr", "127.0.0.1:0", "--parent-pid", str(os.getpid())]
        self.mu = threading.Lock()
        self.proc = None
        self.addr = None

    def connect(self):
        with self.mu:
            if self.proc is None or self.proc.poll() is not None:
                self.proc, self.addr = self._start()
            return HTTPExchange(*self.addr)

    def _start(self):
        proc = subprocess.Popen(self.argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE)
        line = proc.stdout.readline()
        if not line:
            raise exit_error(proc.wait())
        host, port = json.loads(line)["addr"].rsplit(":", 1)
        return proc, (host, int(port))

    def close(self):
        # Called once every session has ended, so nothing is connecting.
        if self.proc is not None and self.proc.poll() is None:
            self.proc.terminate()
            self.proc.wait()


class HTTPExchange:
    """One session over HTTP: a POST per message, on a connection of its own."""

    def __init__(self, host, port):
        self.conn = http.client.HTTPConnection(host, port)
        self.session = ""
        self.mu = threading.Lock()

    def exchange(self, msg):
        msg["session"] = self.session
        with self.mu:
            self.conn.request("POST", "/", json.dumps(msg), {"Content-Type": "application/json"})
            resp = self.conn.getresponse()
            body = resp.read()
        if resp.status == 404:
            raise SessionGone(body.decode().strip())
        if resp.status != 200:
            raise GatewayError("caller" if resp.status < 500 else "gateway",
                               f"HTTP {resp.status}: {body.decode().strip()}")
        reply = json.loads(body)
        if not self.session:
            self.session = reply.get("session", "")
        return reply

    def abort(self):
        # Dropping the connection is hanging up: the gateway stops claiming.
        if self.conn.sock is not None:
            self.conn.sock.shutdown(socket.SHUT_RDWR)

    def close(self):
        self.conn.close()


class StdioExchange:
    """One session over a child's stdin and stdout, one JSON value per line."""

    def __init__(self, argv):
        self.proc = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE)
        self.mu = threading.Lock()

    def exchange(self, msg):
        with self.mu:
            try:
                self.proc.stdin.write((json.dumps(msg) + "\n").encode())
                self.proc.stdin.flush()
            except (BrokenPipeError, ValueError):
                pass  # the read below finds the gateway gone and says why
            line = self.proc.stdout.readline()
        if not line:
            raise exit_error(self.proc.wait())
        return json.loads(line)

    def abort(self):
        # Closing stdin is hanging up: the gateway stops claiming.
        self.proc.stdin.close()

    def close(self):
        if not self.proc.stdin.closed:
            self.proc.stdin.close()
        self.proc.wait()


def exit_error(code):
    """What a gateway's exit code says about the session it carried."""
    if code == 0:
        return SessionGone("gateway exited")
    cls = {75: "transient", 78: "caller"}.get(code, "gateway")
    return GatewayError(cls, f"gateway exited {code}")


class Context:
    """What a function gets besides its work: a way to notice that the worker
    is stopping or to stop it, and reads that answer mid-turn."""

    def __init__(self, xchg, stopping):
        self.xchg = xchg
        self.stopping = stopping

    def stop(self):
        """Stop this worker once the task in hand is settled. Its fate still
        lands, and its on_success or on_dependency still runs."""
        self.stopping.set()

    def sleep(self, seconds):
        """Sleep, waking early if the worker starts stopping. True if it did not."""
        return not self.stopping.wait(seconds)

    def read(self, kind, **fields):
        """Send a read ("time", "tasks", "docs", "queues", "namespaces") and
        return its answer."""
        msg = {"type": kind, **fields}
        if self.stopping.is_set():
            msg["quit"] = True
        return self.xchg.exchange(msg)

    def time(self):
        """The backend's clock, in seconds since the epoch."""
        return self.read("time")["time_ms"] / 1000


class Session:
    """One session: the protocol loop and its switch."""

    def __init__(self, worker, xchg):
        self.w = worker
        self.xchg = xchg
        self.stopping = threading.Event()
        self.ctx = Context(xchg, self.stopping)
        self.keepalive_s = 0
        self.mu = threading.Lock()
        self.abortable = False

    def run(self):
        reply = self.send({"type": "config", "config": self.w.config()})
        if reply["type"] == "ready":
            # Every third of the window, so one late keepalive is not fatal.
            self.keepalive_s = reply.get("silence_timeout_s", 0) / 3
        work = None
        while True:
            kind = reply["type"]
            if kind == "quit":
                return
            if kind == "error":
                raise GatewayError(reply.get("class", "gateway"), reply.get("message", ""))
            if kind == "ready":
                # The gateway is waiting on a claim, and says so now and then.
                msg = {"type": "ready"}
            elif kind == "take":
                work = Work(reply["task"])
                claims, fate = self.settle(self.w.take, work, self.ctx)
                msg = {"type": "take", "doc_set_claims": claims or [], **fate}
            elif kind == "work":
                work = Work(reply["task"], reply.get("doc_sets", []))
                mod, fate = self.settle(self.w.work, work, self.ctx)
                msg = {"type": "modify", **fate}
                if mod is not None:
                    msg["modification"] = mod.request
            elif kind == "success":
                _, fate = self.settle(self.w.on_success, work, self.ctx)
                msg = {"type": "done", **fate}
            elif kind == "dependency":
                _, fate = self.settle(self.w.on_dependency, work, reply.get("deps", []), self.ctx)
                msg = {"type": "done", **fate}
            else:
                raise GatewayError("caller", f"unexpected {kind!r}")
            reply = self.send(msg)

    def send(self, msg):
        """The loop's exchange. Once stopping, every message carries quit, and
        a wait that carries none -- the gateway holds the turn, nothing is in
        hand -- is what stop() aborts."""
        with self.mu:
            if self.stopping.is_set():
                msg["quit"] = True
            self.abortable = not msg.get("quit")
        try:
            return self.xchg.exchange(msg)
        finally:
            with self.mu:
                self.abortable = False

    def stop(self):
        with self.mu:
            self.stopping.set()
            if self.abortable:
                self.xchg.abort()

    def settle(self, fn, *args):
        """Run fn, returning what it returns and, if it raised, the fate that
        asks for: (result, fate)."""
        if fn is None:
            return None, {}
        try:
            return self.keeping_alive(fn, *args), {}
        except RetryError as e:
            return None, {"outcome": "retry", "error": {"message": str(e), "queue": e.or_move_to}}
        except MoveError as e:
            return None, {"outcome": "move", "error": {"message": str(e), "queue": e.queue}}
        except FatalWorker as e:
            return None, {"outcome": "fatal", "error": {"message": str(e)}}
        except Exception as e:
            # A failure the function did not classify stops the worker, after
            # quarantining the task if this was its last claim.
            return None, {"outcome": "error", "error": {"message": f"{type(e).__name__}: {e}"}}

    def keeping_alive(self, fn, *args):
        """Run fn, telling the gateway the client is still working if it runs
        long. Short work sends nothing; work that outlasts one interval keeps
        sending until it returns."""
        if not self.keepalive_s:
            return fn(*args)
        done = threading.Event()

        def keepalive():
            while not done.wait(self.keepalive_s):
                try:
                    self.ctx.read("working")
                except Exception:
                    return  # the session is ending; the answer will say so

        sender = threading.Thread(target=keepalive, daemon=True)
        sender.start()
        try:
            return fn(*args)
        finally:
            # A keepalive sent after the answer would land on the next task.
            done.set()
            sender.join()


class Worker:
    """A worker: its functions and how it registers. It runs as one session."""

    def __init__(self, queues, work, take=None, on_success=None, on_dependency=None,
                 work_timeout_s=0, max_attempts=0, retry_delay_s=0, error_queue=""):
        self.queues = queues
        self.work = work
        self.take = take
        self.on_success = on_success
        self.on_dependency = on_dependency
        self.work_timeout_s = work_timeout_s
        self.max_attempts = max_attempts
        self.retry_delay_s = retry_delay_s
        self.error_queue = error_queue

    def config(self):
        return {
            "queues": self.queues,
            "protocol_versions": PROTOCOLS,
            "want_docs": self.take is not None,
            "want_success": self.on_success is not None,
            "want_dependency": self.on_dependency is not None,
            "work_timeout_s": self.work_timeout_s,
            "max_attempts": self.max_attempts,
            "retry_delay_s": self.retry_delay_s,
            "error_queue": self.error_queue,
        }

    def run(self, **gateway):
        """Run this worker alone; gateway is Gateway's arguments."""
        Gateway(**gateway).run(self)


class Gateway:
    """Runs workers through `eqlink work`, each as a session of its own, and
    stops them together. Over HTTP every session shares one child; over stdio
    each has its own. args are the operator's flags for `eqlink work`, such as
    --lease."""

    def __init__(self, entroq="localhost:37706", transport="http", eqlink="eqlink", args=()):
        self.argv = [eqlink, "--entroq", entroq, "work", *args]
        self.transport = transport
        self.stopping = threading.Event()
        self.mu = threading.Lock()
        self.sessions = set()
        self.failure = None

    def run(self, *workers):
        """Run until every worker has stopped: by ctx.stop, by SIGINT or
        SIGTERM, or by a failure a new session cannot fix. The first signal
        drains; a second hangs up at once."""
        child = None
        if self.transport == "http":
            child = HTTPChild(self.argv)
            connect = child.connect
        else:
            def connect():
                return StdioExchange(self.argv)
        signal.signal(signal.SIGINT, self._on_signal)
        signal.signal(signal.SIGTERM, self._on_signal)
        runners = [threading.Thread(target=self._serve, args=(w, connect)) for w in workers]
        try:
            for r in runners:
                r.start()
            for r in runners:
                r.join()
        finally:
            if child is not None:
                child.close()
        if self.failure is not None:
            raise self.failure

    def stop(self):
        """Drain: every task in hand finishes, and no session claims again."""
        self.stopping.set()
        with self.mu:
            for s in self.sessions:
                s.stop()

    def _on_signal(self, signum, frame):
        if self.stopping.is_set():
            os._exit(130)  # a second signal: hang up, leaving tasks to their leases
        print(f"[worker] {signal.Signals(signum).name}: draining", file=sys.stderr)
        self.stop()

    def _serve(self, worker, connect):
        """Run one worker's sessions one after another until it stops."""
        backoff = 0.2
        while not self.stopping.is_set():
            try:
                xchg = connect()
            except GatewayError as e:
                if not self._recoverable(e):
                    return
                self.stopping.wait(backoff)
                backoff = min(backoff * 2, 5.0)
                continue
            session = Session(worker, xchg)
            with self.mu:
                self.sessions.add(session)
                if self.stopping.is_set():
                    session.stopping.set()
            try:
                session.run()
                backoff = 0.2
            except SessionGone:
                pass
            except (OSError, http.client.HTTPException):
                pass  # an aborted exchange, or a gateway that went away
            except GatewayError as e:
                if not self._recoverable(e):
                    return
                if not session.stopping.is_set():
                    self.stopping.wait(backoff)
                    backoff = min(backoff * 2, 5.0)
            finally:
                with self.mu:
                    self.sessions.discard(session)
                xchg.close()
            if session.stopping.is_set():
                return

    def _recoverable(self, e):
        if e.cls == "transient":
            print(f"[worker] {e}; reconnecting", file=sys.stderr)
            return True
        print(f"[worker] {e}; stopping", file=sys.stderr)
        self.failure = e
        self.stop()
        return False
