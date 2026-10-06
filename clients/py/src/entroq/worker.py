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
from datetime import datetime, timezone, timedelta
from typing import AsyncIterator, Awaitable, Callable, Sequence

from .types import (
    ClaimedDocs, DependencyError, Doc, DocClaim, Modification, ModifyResult,
    Task, TransportError,
)
from .base import EntroQBase

# ---------------------------------------------------------------------------
# Public exception types
# ---------------------------------------------------------------------------

class StopWorker(Exception):
    """Raise from do_work to stop the worker loop cleanly after the current task."""


class FatalWorker(Exception):
    """Raise from do_work or a finisher to stop the worker with this error.

    The task is left alone -- not retried, not quarantined -- so it comes back
    when its lease lapses, for a worker that may be able to do better. Use it for
    something no retry can fix in this process: configuration that cannot be
    read, a dependency that is gone for good.

    It derives from Exception rather than BaseException on purpose. Stopping the
    worker is a decision about this program, not an interruption of it, so code
    that means to catch broad failures should be able to catch this one too.

    Mirrors the Go worker's FatalError. The contrast is StopWorker, which ends
    the loop as a success, and RetryError or MoveError, which dispose of the task
    and carry on.
    """


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
# Claim duration
# ---------------------------------------------------------------------------

# DEFAULT_CLAIM_DURATION_S matches entroq.DefaultClaimDuration in Go, and the
# service's own lease floor: a shorter default is clamped up to it anyway.
# Lengthening a lease costs only the sad path, where a dead holder keeps what
# it held for up to one lease, while the margin it leaves covers latency and
# small clock disagreement on every renewal.
DEFAULT_CLAIM_DURATION_S = 45.0

# Bounds the quarantine EntroQWorker._quarantine_at_limit attempts, which must
# not hold up a worker that is already stopping.
AT_LIMIT_TIMEOUT_S = 5.0


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
        """Return the doc claims needed for this task. Override in subclasses.

        Sentinels act on the task here as they do from ``do_work``: raise
        ``StopWorker`` to exit cleanly, ``RetryError`` to re-queue, or
        ``MoveError`` to send the task to an error queue. Nothing is claimed
        yet, so there are no sets to release.
        """
        return []

    @abstractmethod
    async def do_work(self, task: Task, docs: list[Doc]) -> Modification | None:
        """Process the task.

        Return a ``Modification`` to apply atomically (versions are fixed up
        to the latest renewal), or ``None`` to commit nothing but the release
        of the claimed sets. ``finish()`` runs after the commit either way.

        Raise ``StopWorker`` to exit cleanly, ``RetryError`` to re-queue, or
        ``MoveError`` to send the task to an error queue.
        """

    async def on_success(self, task: Task, docs: list[Doc]) -> None:
        """Called after the body's commit, whatever do_work returned.

        The commit has already landed and the doc claim ended with it, so this
        holds nothing: it is for work that follows the task being handled, not
        for part of handling it. An error here is logged rather than raised,
        since it cannot undo a commit that succeeded; raise ``FatalWorker`` to
        stop the worker anyway, or ``StopWorker`` to exit cleanly.

        This is the Go worker's ``OnSuccess``, and shares its contract. Go's
        ``Handler.Finish`` is a different hook: it owns the commit, and nothing
        implicit happens around it. There is no equivalent of that here.
        """
        await self.finish(task, docs)

    async def finish(self, task: Task, docs: list[Doc]) -> None:
        """The former name of :meth:`on_success`, which calls this by default.

        Overriding it still works and always will. New code should override
        ``on_success``, whose name says when it runs; overriding both runs only
        ``on_success``.
        """

    # ------------------------------------------------------------------
    # Private worker protocol.  EntroQWorker calls these; _FnHandler
    # overrides them to route to its stored functions.
    # ------------------------------------------------------------------

    async def _select(self, task: Task) -> list[DocClaim]:
        return await self.select(task)

    async def _on_success(self, task: Task, docs: list[Doc]) -> None:
        await self.on_success(task, docs)


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

    async def _on_success(self, task: Task, docs: list[Doc]) -> None:
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
        """Decorator: register the function to run after the body's commit.

        It fills the same role as overriding :meth:`Handler.on_success`.

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
    def __init__(self, task: Task, docs: ClaimedDocs) -> None:
        self.task = task
        self.docs = list(docs)
        # The claimed sets, at their lock versions: what a renewal or release
        # names. Separate from docs because a set holds its own lock and may
        # have no members at all.
        self.sets: list[Doc] = list(docs.sets)
        self.error: Exception | None = None
        # Set when the renewer cancelled the work itself, so _process can tell
        # that cancellation apart from one arriving from outside the worker.
        self.work_cancelled = False
        # The in-flight handler task, registered by _process so the renewer can
        # abort work the moment the hold stops being usable.
        self.work: asyncio.Task | None = None
        # Asks the renewer to finish and stand down. It is a request rather
        # than a cancellation because a renewal already in flight may have
        # been applied: see _renewing.
        self.stop = asyncio.Event()

    def stop_work(self, err: Exception) -> None:
        """Record that the hold is gone and cancel the handler.

        Nothing this body produces can be committed against a hold it does not
        have, and it may duplicate whatever the new claimant is doing, so the
        work stops now rather than running to completion.
        """
        self.error = err
        self.work_cancelled = True
        if self.work is not None and not self.work.done():
            self.work.cancel()


def granted_lease_s(task: Task) -> float:
    """Return the lease a claim or renewal actually granted, in seconds.

    At minus Modified. Both are stamped from one backend clock, so the
    difference is exact and owes nothing to this process's clock. The lease
    asked for is not the same number: a service clamps a claim into its own
    bounds, and renewing for the request rather than the grant would leave the
    task free between renewals.
    """
    if task.modified is None:
        return DEFAULT_CLAIM_DURATION_S
    return max(0.0, (task.at - task.modified).total_seconds())


def renewal_interval_s(task: Task) -> float:
    """Return how long to wait between renewals of task: two thirds of the
    lease it was granted, leaving the rest as margin for a renewal to complete
    in. Matches the Go worker, which derives both from one fraction."""
    return granted_lease_s(task) * 2.0 / 3.0


def _renewed(task: Task, sets: Sequence[Doc],
             result: ModifyResult) -> tuple[Task, list[Doc]]:
    """Return the task and sets at the versions a renewal reply reports.

    If a renewal does not come back with what we hold, we do not hold it: a
    version the reply leaves out is a version the caller cannot name again, so
    there is nothing to go on with. That is the lost claim a failed renewal
    already raises, and it is raised the same way here.

    Real docs have IDs and sets do not, so a set's lock is picked out of the
    changed docs by that.
    """
    renewed = [t for t in result.tasks_changed if t.id == task.id]
    if len(renewed) != 1:
        raise DependencyError(
            f"renewal of task {task.id} answered with tasks {result.tasks_changed}")
    locks = {(d.namespace, d.key): d
             for d in result.docs_changed if d.is_set_ref()}
    out: list[Doc] = []
    for group in sets:
        lock = locks.get((group.namespace, group.key))
        if lock is None:
            raise DependencyError(
                f"renewal of task {task.id} did not answer with doc set "
                f"{group.key!r} in {group.namespace!r}")
        out.append(lock)
    return renewed[0], out


# How long to let a renewal already in flight finish once the body has ended.
# It bounds the handoff below, so a wedged server costs a worker this much
# rather than the rest of its life.
RENEWAL_HANDOFF_S = 10.0


@asynccontextmanager
async def _renewing(
    client: EntroQBase,
    task: Task,
    docs: ClaimedDocs,
    since_claim_s: float = 0.0,
) -> AsyncIterator[_RenewState]:
    state = _RenewState(task, docs)

    async def _renewer() -> None:
        # Both the hold and the cadence come from the granted lease, read off
        # the claim itself, so there is no requested duration in scope to renew
        # on by mistake. Computed once: renewing for what is held makes every
        # grant equal the last.
        lease_s = granted_lease_s(task)
        interval_s = renewal_interval_s(task)
        # The lease began at the claim, not here, so whatever reaching the body
        # cost is already gone from it -- claiming doc sets most of all, since
        # that is an all-or-nothing claim against other workers. A first wait
        # of zero renews at once, which is what a claim longer than an interval
        # needs.
        next_s = max(0.0, interval_s - since_claim_s)
        while True:
            if next_s > 0:
                try:
                    await asyncio.wait_for(state.stop.wait(), timeout=next_s)
                    return  # asked to stand down between renewals
                except asyncio.TimeoutError:
                    pass
                except asyncio.CancelledError:
                    return
            next_s = interval_s
            if state.stop.is_set():
                return
            # Past here the renewal runs to completion even if the body ends:
            # a reply in flight may name versions the server has already
            # written, and dropping it loses the only record of them.
            #
            # Arrivals, not changes: a renewal says only "hold this longer",
            # as a duration the backend resolves against its own clock. An
            # instant computed here could arrive already stale, which reads as
            # a release.
            #
            # The sets are named, not their members: a set's lock is what the
            # claim took, and naming it is what lets a set with no docs in it
            # be renewed at all.
            ops: list = [Modification.arriving(state.task, lease_s)]
            for group in state.sets:
                ops.append(Modification.arriving(group, lease_s))
            try:
                result = await client.modify(Modification(*ops))
            except DependencyError as e:
                # The claim is gone. Work done from here cannot be committed,
                # and may duplicate whatever the new claimant is doing, so stop
                # the handler now rather than letting it run to completion.
                state.stop_work(e)
                return
            except Exception as e:
                logging.warning("Renewal error for task %s (will retry): %s", task.id, e)
                continue
            try:
                state.task, state.sets = _renewed(state.task, state.sets, result)
            except DependencyError as e:
                state.stop_work(e)
                return

    renew_task = asyncio.create_task(_renewer())
    try:
        yield state
    finally:
        # Ask the renewer to stand down and wait for it, rather than cancelling
        # it: a renewal in flight may already have been applied, and cancelling
        # there throws away the versions it was about to report. The commit
        # that follows would then name the versions from before it and be
        # refused, discarding work that succeeded. A wedged server is bounded
        # by RENEWAL_HANDOFF_S, after which there is nothing to do but cancel.
        state.stop.set()
        try:
            await asyncio.wait_for(renew_task, timeout=RENEWAL_HANDOFF_S)
        except Exception:
            # The handoff timed out, or the renewal failed on its way out.
            # Either way the hold is as good as it is going to get, and the
            # error the renewer recorded is what says so.
            pass
        finally:
            if not renew_task.done():
                renew_task.cancel()
            await asyncio.gather(renew_task, return_exceptions=True)


# ---------------------------------------------------------------------------
# Internal: version fix-up
# ---------------------------------------------------------------------------

def _fix_versions(mod: Modification, task: Task, docs: list[Doc],
                  sets: list[Doc]) -> None:
    """Mutate mod in place so every version it names is the current one.

    This is the one place a renewal's effect on versions is applied, so
    nothing else has to be kept in step with it.

    The task's version comes from the last renewal of the task. A doc has no
    version of its own -- it carries its set's, the only one it has -- so a
    claimed member takes the version of the set holding it, and every version
    this worker can name is written out rather than left for the service to
    infer.

    Anything mod names that this worker did not claim is left alone: its
    version is not ours to decide. But a doc this worker DID claim must
    resolve, because the only answers are the set's version and a wrong one;
    the claim-time version in particular is exactly what a renewal moves past.
    A member whose set is missing means the claim and the renewal disagree
    about what is held, which is a bug here rather than a condition to paper
    over, so it stops the worker.
    """
    for op in (*mod.task_changes, *mod.task_deletes, *mod.task_depends,
               *mod.task_arrivals):
        if op.id == task.id:
            op.version = task.version

    set_version = {(g.namespace, g.key): g.version for g in sets}
    # A member names itself by ID, which does not say which set holds it, so
    # the claimed members supply that mapping.
    set_of = {(d.namespace, d.id): (d.namespace, d.key) for d in docs}

    def current(namespace: str, id: str) -> int | None:
        group = set_of.get((namespace, id))
        if group is None:
            return None  # not claimed here; the caller owns its version
        if group not in set_version:
            raise FatalWorker(
                f"doc {namespace}/{id} was claimed in set {group[1]!r} but no "
                "lock for that set is held: the claim and the renewal disagree")
        return set_version[group]

    for op in (*mod.doc_changes, *mod.doc_deletes, *mod.doc_depends):
        version = current(op.namespace, op.id)
        if version is not None:
            op.version = version

    # An arrival names its SET directly, so it takes the set's version without
    # going through a member. This is how a handler holds a set past the body
    # (see _releasing), so leaving it at the claim-time version would reject
    # the whole commit after the first renewal.
    for op in mod.doc_arrivals:
        group = (op.namespace, op.key)
        if group in set_version:
            op.version = set_version[group]


def _reset_claims(mod: Modification, task_id: str) -> None:
    """Mark the task's own changes in mod as resetting its claim count.

    The worker changed the task on purpose, so the claims before this one no
    longer point at a poison pill.
    """
    for tc in mod.task_changes:
        if tc.id == task_id:
            tc.reset_claims = True


def _releasing(mod: Modification, sets: Sequence[Doc]) -> None:
    """Add to mod a release for every claimed set whose arrival it has not
    already decided, so the sets go back in the very modification that ends the
    worker body.

    A doc claim is a transaction scoped to that body: the sets were taken for
    the work, the work is over, so they are no longer in it. They go back
    whatever else mod does -- the task need not be in it at all, and a body that
    mutated docs and left the task alone still frees them. Doc sets are shared,
    so an early release unblocks consumers with nothing to do with this task,
    and the task's own arrival says nothing about them.

    Decided means mod names the set in an arrival, or a member write names an
    arrival of its own (at is not None, which is how a write asks to hold its
    set; at=None means ready now). Explicit intent wins: a body that holds a set
    keeps it. Doing nothing releases everything, which is the useful default.

    The result only ever names sets claimed here, and must stay that way:
    releasing moves a set's version, so naming one this worker holds no lease on
    would disturb a set belonging to somebody else or to nobody. The leasehold
    decides what may be released and mod only decides what to leave out.

    Nothing releases on a path where the body failed. A set claimed for a task
    took that task's own arrival, and renewal keeps the two equal, so when
    nothing moved they lapse together and no cleanup could beat the lease.
    """
    if not sets:
        return
    decided = {(a.namespace, a.key) for a in mod.doc_arrivals}
    decided |= {(d.namespace, d.key) for d in mod.doc_inserts if d.at is not None}
    decided |= {(c.namespace, c.key) for c in mod.doc_changes if c.at is not None}
    for group in sets:
        if (group.namespace, group.key) not in decided:
            Modification.arriving(group, 0.0)._apply(mod)


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
        claim_duration_s: float = DEFAULT_CLAIM_DURATION_S,
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

    async def _claim_docs(self, task: Task, handler: Handler) -> ClaimedDocs:
        doc_claims = await handler._select(task)
        if not doc_claims:
            return ClaimedDocs()
        # One all-or-nothing claim of every set. Claiming them one at a time
        # would let two workers each hold part of what both need, so an
        # ordering was required to break the deadlock; taking them together
        # leaves nothing to order.
        #
        # The sets are held until the task arrives rather than for a duration
        # of their own, so they expire exactly with it, from one reading of
        # one clock. The claim fails if the task is no longer at this version,
        # which means this worker no longer holds it.
        return await self._client.claim_doc_sets(doc_claims, task_to_match=task)

    async def _failed(self, task: Task, exc: BaseException, *,
                      sets: Sequence[Doc] = ()) -> bool:
        """Dispose of a failure from a handler phase, and say what to do next.

        One ladder for every phase, the selector's and the body's, so they
        cannot drift: a sentinel acts on the task, and anything else stops the
        worker -- quarantining the task first if this was its last allowed
        claim, so the reason survives. The return value is what ``_process``
        returns: ``False`` to stop the worker loop, ``True`` to go on.
        """
        if isinstance(exc, StopWorker):
            return False  # the claim expires naturally
        if isinstance(exc, (RetryError, MoveError)):
            await self._dispose(task, exc, sets=sets)
            return True
        if isinstance(exc, FatalWorker):
            raise exc
        await self._quarantine_at_limit(task, sets, exc)
        raise exc

    async def _quarantine_at_limit(self, task: Task | None, sets: Sequence[Doc],
                                   exc: BaseException) -> bool:
        """Move task to its error queue now if this was its last allowed claim.

        A handler failure below the claim limit records nothing: a record is a
        modification, and a modification resets the claim count, so the task
        waits out its lease as it did before. At the limit the next claim would
        move it anyway without saying why, so the reason is written down while
        it is still known.

        Best-effort, with its own short timeout, since the caller is stopping
        either way: a failure here is logged and the caller's exception still
        propagates. Returns whether the task moved.
        """
        if self._max_claims <= 0 or task is None or task.claims < self._max_claims:
            return False
        reason = MoveError(f"handler failed at the claim limit ({task.claims} claims, "
                           f"limit {self._max_claims}): {exc}")
        try:
            await asyncio.wait_for(self._dispose(task, reason, sets=sets),
                                   timeout=AT_LIMIT_TIMEOUT_S)
        except Exception as e:
            logging.warning("Quarantine of task %s at its claim limit: %s", task.id, e)
            return False
        return True

    async def _dispose(self, task: Task, exc: RetryError | MoveError, *,
                       sets: Sequence[Doc] = (), jitter: bool = False) -> None:
        """Apply a sentinel's disposition to a claimed task, releasing its sets.

        Retry and quarantine are one decision, taken here at the moment of
        failure, as the Go client's ``RetryOrQuarantine`` does. Deferring the
        ceiling check to the next claim would mean an exhausted task is only
        quarantined if a worker happens to come back for it, and none may.

        ``jitter`` spreads a retry's delay by up to a quarter, for a failure
        every loser of one race suffers at the same instant. Without it they
        all come back together and all but one lose again. Go's
        contentionDelay does the same, and only there.
        """
        attempt = task.attempt + 1

        def quarantine(dest: str) -> Modification:
            # at=None releases the task immediately: a quarantined task is for
            # inspection, so a retry delay must not leak into its arrival time.
            #
            # reset_claims because this failure was HANDLED: the claims before
            # it do not point at a poison pill, so they must not count toward
            # the claim limit. Go's RetryOrQuarantine resets on every
            # disposition for the same reason.
            return Modification.changing(task, queue=dest, at=None,
                                         attempt=attempt, err=str(exc),
                                         reset_claims=True)

        if isinstance(exc, MoveError):
            dest = exc.queue or self.error_queue_for(task.queue)
            if not dest:
                return  # custom map declined a destination: claim expires
            change = quarantine(dest)
        elif self._max_attempts and attempt >= self._max_attempts:
            change = quarantine(exc.or_move_to or self.error_queue_for(task.queue))
        else:
            delay = exc.delay_s if exc.delay_s is not None else self._retry_delay_s
            if jitter:
                delay += random.uniform(0, delay / 4)
            at = datetime.now(tz=timezone.utc) + timedelta(seconds=delay)
            change = Modification.changing(task, at=at, attempt=attempt,
                                           err=str(exc), reset_claims=True)

        ops = [change]
        for group in sets:
            ops.append(Modification.arriving(group, 0.0))
        await self._client.modify(Modification(*ops))

    async def _process(self, task: Task, handler: Handler) -> bool:
        """Process one already-claimed task. Returns False if the worker should stop."""
        # The task's lease is already running: it started when the claim
        # stamped it, which is just before this call. What the ceiling check,
        # the selector and the doc claim spend comes out of the lease, so the
        # renewer is told how much of it is gone.
        claimed_at = asyncio.get_running_loop().time()
        # Claim count is checked first: a task over the claim ceiling has been
        # wedging workers, so it must not reach the handler at all.
        if self._max_claims > 0 and task.claims > self._max_claims:
            dest = self.error_queue_for(task.queue)
            # The same disposition Go applies here: the attempt counts, the
            # reason is recorded, and the claim count resets, so a task that is
            # inspected and re-queued starts its budget over rather than
            # tripping the limit again on its first claim.
            await self._client.modify(Modification(
                Modification.changing(
                    task, queue=dest, at=None,
                    attempt=task.attempt + 1,
                    err=f"maximum claims exceeded ({self._max_claims})",
                    reset_claims=True),
            ))
            return True

        try:
            docs = await self._claim_docs(task, handler)
        except DependencyError as e:
            # Classify rather than swallow, and record the reason on the task
            # instead of letting it vanish into a log line. A missing doc can
            # never be claimed, so the task is a poison pill. A doc held by
            # someone else is transient. A failed depend on the task itself
            # means this worker ran past its own lease while taking docs --
            # the sets are held until the task arrives, so the hold it asked
            # for was already behind it -- which is transient too, and worth
            # saying apart from contention.
            if e.has_missing_docs():
                await self._dispose(task, MoveError(f"required doc missing: {e}"))
            elif e.has_missing():
                await self._dispose(task, RetryError(f"task lease lapsed while taking docs: {e}"),
                                    jitter=True)
            else:
                await self._dispose(task, RetryError(f"doc contention: {e}"),
                                    jitter=True)
            return True
        except Exception as e:
            # Nothing is claimed yet, so there are no sets to release.
            return await self._failed(task, e)

        do_work_exc: BaseException | None = None
        result: Modification | None = None

        since_claim_s = asyncio.get_running_loop().time() - claimed_at
        async with _renewing(self._client, task, docs, since_claim_s) as state:
            work = asyncio.ensure_future(handler.do_work(task, docs))
            state.work = work
            try:
                result = await work
            except asyncio.CancelledError:
                # Only swallow a cancellation the renewer caused; anything
                # else is the caller stopping us and must keep propagating.
                if not state.work_cancelled:
                    raise
            except Exception as e:
                do_work_exc = e

        # Three outcomes in Go's order. A fatal keeps the handler's control
        # whatever else happened. Then a stopped renewal is why the work
        # ended, whatever the handler returned: it is a lost claim, which the
        # run loop logs and goes on from, and not the handler's failure, so
        # nothing is quarantined for it. Only then does the handler's own
        # failure decide.
        if isinstance(do_work_exc, FatalWorker):
            raise do_work_exc
        if state.error:
            raise state.error
        if do_work_exc is not None:
            return await self._failed(state.task, do_work_exc, sets=state.sets)

        # The modification that ends the body carries the releases. A body
        # that decided nothing still ended, and its sets still go back, in a
        # modification of their own.
        mod = result if result is not None else Modification()
        _fix_versions(mod, state.task, state.docs, state.sets)
        _reset_claims(mod, state.task.id)
        _releasing(mod, state.sets)
        # A backend refuses a modification naming no operation, so a body that
        # claimed nothing and decided nothing writes nothing.
        if not mod.is_empty():
            await self._client.modify(mod)

        # on_success holds no doc claim: the transaction ended with the commit.
        #
        # Its failure is optimistic, as the Go worker's OnSuccess is: the task is
        # already committed, so an error here cannot undo that and must not
        # discard it either. Logged and carried on, unless it is fatal.
        try:
            await handler._on_success(state.task, state.docs)
        except StopWorker:
            return False
        except FatalWorker:
            raise
        except Exception as e:
            logging.warning("Finisher for task %s failed after the commit: %s",
                            state.task.id, e)

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
