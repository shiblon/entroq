"""EntroQ async worker framework.

This is a port of Go's ``pkg/worker``, which is the reference implementation for
EntroQ worker semantics. Match it rather than inventing behavior here; see
"The Go worker is the reference implementation" in AGENTS.md.
"""
from __future__ import annotations

import asyncio
import logging
import random
from abc import ABC, abstractmethod
from contextlib import asynccontextmanager
from dataclasses import dataclass
from datetime import datetime, timezone, timedelta
from typing import AsyncIterator, Callable, Awaitable

from .types import Task, Doc, DependencyError, Modification, TransportError
from .base import EntroQBase

# ---------------------------------------------------------------------------
# Public exception types
# ---------------------------------------------------------------------------

class StopWorker(Exception):
    """Raise from do_work to stop the worker loop cleanly after the current task."""


class RetryError(Exception):
    """Raise from do_work to re-queue the task for retry after a delay.

    ``delay_s`` overrides the worker's ``retry_delay_s`` for this failure.
    ``or_move_to`` overrides the queue the task is quarantined to once it
    exhausts ``max_attempts``; it has no effect on the retries themselves.
    Mirrors the Go client's ``RetryError.After`` and ``RetryError.OrMoveTo``.
    """

    def __init__(self, message: str = "", *, delay_s: float | None = None,
                 or_move_to: str = "") -> None:
        super().__init__(message)
        self.delay_s = delay_s
        self.or_move_to = or_move_to


class MoveError(Exception):
    """Raise from do_work to move the task to an error queue."""

    def __init__(self, message: str = "", *, queue: str = "") -> None:
        super().__init__(message)
        self.queue = queue


# ---------------------------------------------------------------------------
# Error queue mapping
# ---------------------------------------------------------------------------

def default_err_q_map(inbox: str) -> str:
    """Return the default error queue for an inbox: the inbox plus ``/err``.

    This matches the Go worker's ``DefaultErrQMap`` so that both clients
    quarantine to the same place by default.
    """
    return inbox + '/err'


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
        # Set by the renewer when the claim is lost, so _process can tell its
        # own cancellation apart from one arriving from outside the worker.
        self.claim_lost = False
        # The in-flight handler task, registered by _process so the renewer can
        # abort work the moment the claim stops being ours.
        self.work: asyncio.Task | None = None

    def lose_claim(self, err: Exception) -> None:
        """Record claim loss and cancel the handler if one is running."""
        self.error = err
        self.claim_lost = True
        if self.work is not None and not self.work.done():
            self.work.cancel()


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
                # The claim is gone. Work done from here cannot be committed,
                # and may duplicate whatever the new claimant is doing, so stop
                # the handler now rather than letting it run to completion.
                state.lose_claim(e)
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

        @EntroQWorker.handler
        async def process(task, docs):
            return Modification(Modification.deleting(task))

        async def main():
            async with EntroQJSON('http://localhost:8080') as eq:
                worker = EntroQWorker(eq, 'my-queue')
                await worker.run(process)

        asyncio.run(main())
    """

    def __init__(
        self,
        client: EntroQBase,
        *queues: str,
        claim_duration_s: float = 30.0,
        err_queue: str = '',
        err_q_map: Callable[[str], str] | None = None,
        retry_delay_s: float = 30.0,
        max_attempts: int = 0,
        max_claims: int = 0,
        retry_transport_errors: bool = True,
        transport_retry_base_s: float = 1.0,
        transport_retry_max_s: float = 30.0,
    ) -> None:
        """Configure a worker.

        Failures are quarantined to an error queue chosen by
        :meth:`error_queue_for`. Pass ``err_queue`` to send every failure to one
        fixed queue, or ``err_q_map`` to compute it per inbox (the Go worker's
        ``WithErrQMap``); they are mutually exclusive. With neither,
        :func:`default_err_q_map` appends ``/err`` to the task's own queue.

        ``max_claims`` bounds how many times a task may be claimed before it
        is moved to the error queue without invoking the handler; it catches
        tasks that crash or wedge their worker, which ``max_attempts`` (driven
        by :class:`RetryError`) cannot see. 0 (the default) means no maximum.

        Safe, pre-submit claim transport failures retry with full-jitter
        exponential backoff by default. Ambiguous transport failures and
        handler exceptions propagate to :meth:`run`'s caller. Set
        ``retry_transport_errors=False`` when the caller owns all retry policy.
        """
        if transport_retry_base_s < 0:
            raise ValueError("transport_retry_base_s must be non-negative")
        if transport_retry_max_s < transport_retry_base_s:
            raise ValueError("transport_retry_max_s must be at least transport_retry_base_s")
        if err_queue and err_q_map is not None:
            raise ValueError("err_queue and err_q_map are mutually exclusive")
        self._client = client
        self._queues = list(queues)
        self._claim_duration_s = claim_duration_s
        if err_queue:
            self._err_q_map: Callable[[str], str] = lambda inbox: err_queue
        else:
            self._err_q_map = err_q_map if err_q_map is not None else default_err_q_map
        self._retry_delay_s = retry_delay_s
        self._max_attempts = max_attempts
        self._max_claims = max_claims
        self._retry_transport_errors = retry_transport_errors
        self._transport_retry_base_s = transport_retry_base_s
        self._transport_retry_max_s = transport_retry_max_s
        self._stop_event = asyncio.Event()

    def stop(self) -> None:
        """Signal the worker to stop. Unblocks any waiting claim() immediately."""
        self._stop_event.set()

    def error_queue_for(self, inbox: str) -> str:
        """Return the error queue for an inbox, using this worker's mapping.

        Mirrors the Go worker's ``ErrorQueueFor``.
        """
        return self._err_q_map(inbox)

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

    async def _dispose(self, task: Task, docs: list[Doc],
                       exc: RetryError | MoveError) -> None:
        """Apply a sentinel's disposition to a claimed task, releasing its docs.

        Retry and quarantine are one decision, taken here at the moment of
        failure, as the Go client's ``RetryOrQuarantine`` does. Deferring the
        ceiling check to the next claim would mean an exhausted task is only
        quarantined if a worker happens to come back for it, and none may.
        """
        attempt = task.attempt + 1

        def quarantine(dest: str) -> Modification:
            # at=None releases the task immediately: a quarantined task is for
            # inspection, so a retry delay must not leak into its arrival time.
            return Modification.changing(task, queue=dest, at=None,
                                         attempt=attempt, err=str(exc))

        if isinstance(exc, MoveError):
            dest = exc.queue or self.error_queue_for(task.queue)
            if not dest:
                return  # custom map declined a destination: claim expires
            change = quarantine(dest)
        elif self._max_attempts and attempt >= self._max_attempts:
            change = quarantine(exc.or_move_to or self.error_queue_for(task.queue))
        else:
            delay = exc.delay_s if exc.delay_s is not None else self._retry_delay_s
            at = datetime.now(tz=timezone.utc) + timedelta(seconds=delay)
            change = Modification.changing(task, at=at, attempt=attempt, err=str(exc))

        ops = [change]
        for doc in docs:
            ops.append(Modification.changing(doc, at=None))
        await self._client.modify(Modification(*ops))

    async def _process(self, task: Task, handler: Handler) -> bool:
        """Process one already-claimed task. Returns False if the worker should stop."""
        # Claim count is checked first: a task over the claim ceiling has been
        # wedging workers, so it must not reach the handler at all.
        if self._max_claims > 0 and task.claims > self._max_claims:
            dest = self.error_queue_for(task.queue)
            await self._client.modify(Modification(
                Modification.changing(task, queue=dest,
                                      err=f"maximum claims exceeded ({self._max_claims})"),
            ))
            return True

        try:
            docs = await self._claim_docs(task, handler)
        except DependencyError as e:
            # Classify rather than swallow: a missing doc can never be claimed,
            # so the task is a poison pill; a doc held by someone else is
            # transient and deserves a backoff. Either way the reason is
            # recorded on the task instead of vanishing into a log line.
            if e.has_missing_docs():
                await self._dispose(task, [], MoveError(f"required doc missing: {e}"))
            else:
                await self._dispose(task, [], RetryError(f"doc contention: {e}"))
            return True

        do_work_exc: Exception | None = None
        result: Modification | None = None

        async with _renewing(self._client, task, docs, self._claim_duration_s) as state:
            work = asyncio.ensure_future(handler.do_work(task, docs))
            state.work = work
            try:
                result = await work
            except (StopWorker, RetryError, MoveError) as e:
                do_work_exc = e
            except asyncio.CancelledError:
                # Only swallow a cancellation the renewer caused; anything else
                # is the caller stopping us and must keep propagating.
                if not state.claim_lost:
                    raise
            # Other exceptions propagate; the renewer is still cleaned up.

        if state.error:
            raise state.error

        if isinstance(do_work_exc, StopWorker):
            return False  # claim expires naturally

        if isinstance(do_work_exc, (RetryError, MoveError)):
            await self._dispose(state.task, state.docs, do_work_exc)
            return True

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
        await self._run_loop(handler)

    async def _run_loop(self, handler: Handler) -> None:
        retry_cap = self._transport_retry_base_s
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
            except TransportError as e:
                if not self._retry_transport_errors or not e.safe_to_retry:
                    raise
                delay = random.uniform(0, retry_cap)
                logging.warning("Claim transport failed; retrying in %.2fs: %s", delay, e)
                retry_cap = min(self._transport_retry_max_s, max(retry_cap * 2, self._transport_retry_base_s))
                try:
                    await asyncio.wait_for(self._stop_event.wait(), timeout=delay)
                except asyncio.TimeoutError:
                    pass
                continue
            retry_cap = self._transport_retry_base_s

            try:
                if not await self._process(task, handler):
                    break
            except asyncio.CancelledError:
                raise
            except DependencyError as e:
                logging.warning("Dependency error, continuing: %s", e)
