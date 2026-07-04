"""EntroQ async worker framework."""
from __future__ import annotations

import asyncio
import logging
from abc import ABC, abstractmethod
from contextlib import asynccontextmanager
from dataclasses import dataclass
from datetime import datetime, timezone, timedelta
from typing import AsyncIterator, Callable, Awaitable

from .types import Task, Doc, DependencyError, Modification
from .base import EntroQBase


# ---------------------------------------------------------------------------
# Ambient garbage collection
#
# GC is a first-class, always-on EntroQ behavior owned by the backend. The Go
# server GCs itself, so a worker talking to it over HTTP/gRPC must not. A worker
# wired to a *direct* backend (the PostgreSQL client) is the only thing running
# against that database, so it carries GC on the backend's behalf. We detect
# that case by capability -- the client exposes gc_collect() -- and drive it
# invisibly from run(); no config, no knobs. These internal tunings mirror the
# Go backends' defaultGCInterval / defaultGCBatchSize.
# ---------------------------------------------------------------------------

_GC_INTERVAL_S = 60.0
_GC_BATCH = 1000


# ---------------------------------------------------------------------------
# Public exception types
# ---------------------------------------------------------------------------

class StopWorker(Exception):
    """Raise from do_work to stop the worker loop cleanly after the current task."""


class RetryError(Exception):
    """Raise from do_work to re-queue the task for retry after a delay."""

    def __init__(self, message: str = "", *, delay_s: float | None = None) -> None:
        super().__init__(message)
        self.delay_s = delay_s


class MoveError(Exception):
    """Raise from do_work to move the task to an error queue."""

    def __init__(self, message: str = "", *, queue: str = "") -> None:
        super().__init__(message)
        self.queue = queue


# ---------------------------------------------------------------------------
# DocClaim
# ---------------------------------------------------------------------------

@dataclass
class DocClaim:
    """Describes a set of docs (by namespace + key) to claim atomically for a task."""
    namespace: str
    key: str
    duration_s: float = 30.0


# ---------------------------------------------------------------------------
# Handler ABC
# ---------------------------------------------------------------------------

class Handler(ABC):
    """Abstract base for task handlers.

    Implement ``do_work`` (required). Override ``select`` to declare which docs
    to claim before work begins, and ``finish`` for custom finalization when
    ``do_work`` returns ``None``.

    For a functional style, use the ``@EntroQWorker.handler`` decorator and
    chain ``@h.selector`` / ``@h.finisher`` onto it, property-style — always
    reusing the same name::

        @EntroQWorker.handler
        async def process(task, docs):
            return Modification(Modification.deleting(task))

        @process.selector
        async def process(task):
            return [DocClaim('config', task.queue + '/settings')]

        @process.finisher
        async def process(task, docs):
            ...

        worker = EntroQWorker(eq, 'my-queue')
        asyncio.run(worker.run(process))
    """

    async def select(self, task: Task) -> list[DocClaim]:
        """Return the doc claims needed for this task. Override in subclasses."""
        return []

    @abstractmethod
    async def do_work(self, task: Task, docs: list[Doc]) -> Modification | None:
        """Process the task.

        Return a ``Modification`` to apply atomically (versions are fixed up
        to the latest renewal), or ``None`` to delegate to ``finish()``.

        Raise ``StopWorker`` to exit cleanly, ``RetryError`` to re-queue, or
        ``MoveError`` to send the task to an error queue.
        """

    async def finish(self, task: Task, docs: list[Doc]) -> None:
        """Called when do_work returns None. Override in subclasses to finalize."""

    # ------------------------------------------------------------------
    # Private worker protocol.  EntroQWorker calls these; _FnHandler
    # overrides them to route to its stored functions.
    # ------------------------------------------------------------------

    async def _select(self, task: Task) -> list[DocClaim]:
        return await self.select(task)

    async def _do_finish(self, task: Task, docs: list[Doc]) -> None:
        await self.finish(task, docs)


# ---------------------------------------------------------------------------
# Functional handler built by @EntroQWorker.handler
# ---------------------------------------------------------------------------

class _FnHandler(Handler):
    """Handler assembled from plain async functions via the decorator API."""

    def __init__(
        self,
        do_work_fn: Callable[[Task, list[Doc]], Awaitable[Modification | None]],
        *,
        selector_fn: Callable[[Task], Awaitable[list[DocClaim]]] | None = None,
        finisher_fn: Callable[[Task, list[Doc]], Awaitable[None]] | None = None,
    ) -> None:
        self._do_work_fn = do_work_fn
        self._selector_fn = selector_fn
        self._finisher_fn = finisher_fn

    async def do_work(self, task: Task, docs: list[Doc]) -> Modification | None:
        return await self._do_work_fn(task, docs)

    async def _select(self, task: Task) -> list[DocClaim]:
        return await self._selector_fn(task) if self._selector_fn is not None else []

    async def _do_finish(self, task: Task, docs: list[Doc]) -> None:
        if self._finisher_fn is not None:
            await self._finisher_fn(task, docs)

    def selector(
        self,
        fn: Callable[[Task], Awaitable[list[DocClaim]]],
    ) -> _FnHandler:
        """Decorator: register the async doc-selection function.

        Use the same name as the handler (property-style)::

            @process.selector
            async def process(task):
                return [DocClaim('ns', 'key')]
        """
        return _FnHandler(self._do_work_fn, selector_fn=fn, finisher_fn=self._finisher_fn)

    def finisher(
        self,
        fn: Callable[[Task, list[Doc]], Awaitable[None]],
    ) -> _FnHandler:
        """Decorator: register the async finalization function.

        Use the same name as the handler (property-style)::

            @process.finisher
            async def process(task, docs):
                ...
        """
        return _FnHandler(self._do_work_fn, selector_fn=self._selector_fn, finisher_fn=fn)


# ---------------------------------------------------------------------------
# Internal: renewal state and context manager
# ---------------------------------------------------------------------------

class _RenewState:
    def __init__(self, task: Task, docs: list[Doc]) -> None:
        self.task = task
        self.docs = list(docs)
        self.error: Exception | None = None


@asynccontextmanager
async def _renewing(
    client: EntroQBase,
    task: Task,
    docs: list[Doc],
    duration_s: float,
) -> AsyncIterator[_RenewState]:
    state = _RenewState(task, docs)

    async def _renewer() -> None:
        while True:
            try:
                await asyncio.sleep(duration_s / 2)
            except asyncio.CancelledError:
                return
            at = datetime.now(tz=timezone.utc) + timedelta(seconds=duration_s)
            ops: list = [Modification.changing(state.task, at=at)]
            for doc in state.docs:
                ops.append(Modification.changing(doc, at=at))
            try:
                result = await client.modify(Modification(*ops))
                if result.tasks_changed:
                    state.task = result.tasks_changed[0]
                if result.docs_changed:
                    updated = {(d.namespace, d.id): d for d in result.docs_changed}
                    state.docs = [updated.get((d.namespace, d.id), d) for d in state.docs]
            except DependencyError as e:
                state.error = e
                return
            except Exception as e:
                logging.warning("Renewal error for task %s (will retry): %s", task.id, e)

    renew_task = asyncio.create_task(_renewer())
    try:
        yield state
    finally:
        renew_task.cancel()
        await asyncio.gather(renew_task, return_exceptions=True)


# ---------------------------------------------------------------------------
# Internal: version fix-up
# ---------------------------------------------------------------------------

def _fix_versions(mod: Modification, task: Task, docs: list[Doc]) -> None:
    """Mutate mod in place to use the latest task/doc versions from renewal."""
    task_ver = {task.id: task.version}
    for tc in mod.task_changes:
        if tc.id in task_ver:
            tc.version = task_ver[tc.id]
    for td in mod.task_deletes:
        if td.id in task_ver:
            td.version = task_ver[td.id]
    for td in mod.task_depends:
        if td.id in task_ver:
            td.version = task_ver[td.id]

    doc_ver = {(d.namespace, d.id): d.version for d in docs}
    for dc in mod.doc_changes:
        k = (dc.namespace, dc.id)
        if k in doc_ver:
            dc.version = doc_ver[k]
    for dd in mod.doc_deletes:
        k = (dd.namespace, dd.id)
        if k in doc_ver:
            dd.version = doc_ver[k]
    for dd in mod.doc_depends:
        k = (dd.namespace, dd.id)
        if k in doc_ver:
            dd.version = doc_ver[k]


# ---------------------------------------------------------------------------
# EntroQWorker
# ---------------------------------------------------------------------------

class EntroQWorker:
    """Async worker: claims tasks from queues and dispatches to a Handler.

    Example::

        eq = EntroQJSON('http://localhost:8080')

        @EntroQWorker.handler
        async def process(task, docs):
            return Modification(Modification.deleting(task))

        @process.selector
        async def process(task):
            return [DocClaim('config', task.queue + '/settings')]

        worker = EntroQWorker(eq, 'my-queue')
        asyncio.run(worker.run(process))
    """

    def __init__(
        self,
        client: EntroQBase,
        *queues: str,
        claim_duration_s: float = 30.0,
        err_queue: str = '',
        retry_delay_s: float = 30.0,
        max_attempts: int = 0,
    ) -> None:
        self._client = client
        self._queues = list(queues)
        self._claim_duration_s = claim_duration_s
        self._err_queue = err_queue
        self._retry_delay_s = retry_delay_s
        self._max_attempts = max_attempts
        self._stop_event = asyncio.Event()

    def stop(self) -> None:
        """Signal the worker to stop. Unblocks any waiting claim() immediately."""
        self._stop_event.set()

    @classmethod
    def handler(
        cls,
        fn: Callable[[Task, list[Doc]], Awaitable[Modification | None]],
    ) -> _FnHandler:
        """Decorator: build a Handler from an async do_work function.

        Chain ``@h.selector`` and ``@h.finisher`` onto the result using the
        same name (property-style)::

            @EntroQWorker.handler
            async def process(task, docs):
                return Modification(Modification.deleting(task))

            @process.selector
            async def process(task):
                return [DocClaim('ns', 'key')]

            worker = EntroQWorker(eq, 'my-queue')
            asyncio.run(worker.run(process))
        """
        return _FnHandler(fn)

    async def _claim_docs(self, task: Task, handler: Handler) -> list[Doc]:
        doc_claims = await handler._select(task)
        if not doc_claims:
            return []
        # Sort to avoid dining-philosopher livelock when multiple workers
        # race to claim overlapping doc sets.
        sorted_claims = sorted(doc_claims, key=lambda d: (d.namespace, d.key))
        docs: list[Doc] = []
        for dc in sorted_claims:
            claimed = await self._client.claim_docs(
                dc.namespace, dc.key, duration_ms=int(dc.duration_s * 1000)
            )
            docs.extend(claimed)
        return docs

    async def _process(self, task: Task, handler: Handler) -> bool:
        """Process one already-claimed task. Returns False if the worker should stop."""
        if self._max_attempts and task.attempt >= self._max_attempts:
            dest = self._err_queue or task.queue + '/error'
            await self._client.modify(Modification(
                Modification.changing(task, queue=dest,
                                      err=f"max attempts ({self._max_attempts}) exceeded"),
            ))
            return True

        docs = await self._claim_docs(task, handler)

        do_work_exc: Exception | None = None
        result: Modification | None = None

        async with _renewing(self._client, task, docs, self._claim_duration_s) as state:
            try:
                result = await handler.do_work(task, docs)
            except (StopWorker, RetryError, MoveError) as e:
                do_work_exc = e
            # Other exceptions propagate; the renewer is still cleaned up.

        if state.error:
            raise state.error

        if isinstance(do_work_exc, StopWorker):
            return False  # claim expires naturally

        if isinstance(do_work_exc, RetryError):
            delay = do_work_exc.delay_s if do_work_exc.delay_s is not None else self._retry_delay_s
            at = datetime.now(tz=timezone.utc) + timedelta(seconds=delay)
            ops = [Modification.changing(state.task, at=at,
                                         attempt=state.task.attempt + 1,
                                         err=str(do_work_exc))]
            for doc in state.docs:
                ops.append(Modification.changing(doc, at=None))
            await self._client.modify(Modification(*ops))
            return True

        if isinstance(do_work_exc, MoveError):
            dest = do_work_exc.queue or self._err_queue
            if dest:
                ops = [Modification.changing(state.task, queue=dest, err=str(do_work_exc))]
                for doc in state.docs:
                    ops.append(Modification.changing(doc, at=None))
                await self._client.modify(Modification(*ops))
            return True  # no dest: claim expires naturally

        if result is not None:
            _fix_versions(result, state.task, state.docs)
            await self._client.modify(result)
        try:
            await handler._do_finish(state.task, state.docs)
        except StopWorker:
            return False

        return True

    async def run(self, handler: Handler) -> None:
        """Run the worker loop until stopped, cancelled, or StopWorker is raised.

        ``stop()`` unblocks any waiting ``claim()`` immediately and exits after
        the current task (if any) completes. Cancelling the asyncio task exits
        at the next await point.
        """
        self._stop_event.clear()
        gc_task = (
            asyncio.create_task(self._gc_loop())
            if hasattr(self._client, 'gc_collect')
            else None
        )
        try:
            await self._run_loop(handler)
        finally:
            if gc_task is not None:
                gc_task.cancel()
                await asyncio.gather(gc_task, return_exceptions=True)

    async def _gc_loop(self) -> None:
        """Reap due GC-eligible tasks on the backend's behalf until stopped.

        Runs only when the client is a direct backend exposing gc_collect (see
        the module note above). Each pass tight-drains full batches so a backlog
        clears quickly, then idles until the next interval or the stop signal,
        whichever comes first. Errors are logged, never fatal: GC is best-effort.
        """
        gc_collect = self._client.gc_collect
        while not self._stop_event.is_set():
            try:
                while await gc_collect(_GC_BATCH) >= _GC_BATCH:
                    pass
            except asyncio.CancelledError:
                raise
            except Exception as e:
                logging.exception("GC collect failed, will retry: %s", e)
            try:
                await asyncio.wait_for(self._stop_event.wait(), timeout=_GC_INTERVAL_S)
            except asyncio.TimeoutError:
                pass

    async def _run_loop(self, handler: Handler) -> None:
        while not self._stop_event.is_set():
            # Race claim() against the stop signal so stop() can unblock a
            # claim() that is waiting indefinitely for a task to appear.
            claim_task = asyncio.create_task(
                self._client.claim(
                    self._queues,
                    duration_ms=int(self._claim_duration_s * 1000),
                )
            )
            stop_task = asyncio.create_task(self._stop_event.wait())
            try:
                await asyncio.wait({claim_task, stop_task}, return_when=asyncio.FIRST_COMPLETED)
            except BaseException:
                claim_task.cancel()
                stop_task.cancel()
                raise
            finally:
                stop_task.cancel()
                await asyncio.gather(stop_task, return_exceptions=True)

            if not claim_task.done():
                # Stop signal won; cancel the in-flight claim and exit.
                claim_task.cancel()
                await asyncio.gather(claim_task, return_exceptions=True)
                break

            try:
                task = claim_task.result()
            except Exception as e:
                logging.exception("Claim failed: %s", e)
                await asyncio.sleep(1)
                continue

            try:
                if not await self._process(task, handler):
                    break
            except asyncio.CancelledError:
                raise
            except DependencyError as e:
                logging.warning("Dependency error, continuing: %s", e)
            except Exception as e:
                logging.exception("Worker error, retrying after delay: %s", e)
                await asyncio.sleep(1)
