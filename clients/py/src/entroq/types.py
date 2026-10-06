from __future__ import annotations

import json
import warnings
from abc import ABC, abstractmethod
from collections.abc import Iterable
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Optional, List, Any, Union

# Sentinel used when at=None on a DocChange: epoch is < 1 year ago, so the
# PostgreSQL backend snaps it to now() and clears the claimant (releases).
_DOC_RELEASE_AT = datetime(1970, 1, 1, tzinfo=timezone.utc)

@dataclass
class TaskID:
    """Identifies a specific version of a task (for deletes and depends)."""
    id: str
    version: int
    queue: str = ""

@dataclass
class TaskData:
    """Input spec for a new task insert."""
    queue: str
    at: Optional[datetime] = None   # None -> use backend current time
    value: Any = None
    attempt: int = 0
    err: str = ''
    id: Optional[str] = None        # None -> auto-generate ID

@dataclass
class TaskChange:
    """Specifies new values for an existing task (identified by id + version).

    from_queue is the task's current queue: the source, part of the modify key,
    which the backend matches against stored state. queue is the destination
    (equal to from_queue for a plain change, different for a move). Prefer
    Task.as_change, which fills from_queue from the task automatically.
    """
    id: str
    version: int
    queue: str
    at: Optional[datetime] = None   # None -> use backend current time and release
    from_queue: str = ''
    value: Any = None
    attempt: int = 0
    err: str = ''
    # reset_claims zeroes the task's claim count along with the change. A task
    # changed on purpose is not the one whose repeated claims suggested a
    # poison pill, so the count that would quarantine it starts over.
    reset_claims: bool = False

@dataclass
class Task:
    """A complete task object."""
    id: str
    version: int
    queue: str
    at: datetime
    claimant: str
    value: Any
    created: Optional[datetime] = None
    modified: Optional[datetime] = None
    claims: int = 0
    attempt: int = 0
    err: str = ""

    def as_id(self) -> TaskID:
        return TaskID(self.id, self.version, self.queue)

    def as_change(self, **overrides) -> TaskChange:
        """Return a TaskChange for this task with optional field overrides.

        from_queue is fixed to the task's current queue (the source); overriding
        'queue' moves the task to a new destination. By default the change is
        immediately available and releases the claim; pass ``at`` explicitly to
        renew or defer it.
        """
        return TaskChange(
            id=self.id,
            version=self.version,
            from_queue=self.queue,
            queue=overrides.get('queue', self.queue),
            at=overrides.get('at', None),
            value=overrides.get('value', self.value),
            attempt=overrides.get('attempt', self.attempt),
            reset_claims=overrides.get('reset_claims', False),
            err=overrides.get('err', self.err),
        )

@dataclass
class DocID:
    """Identifies a specific version of a doc, or of a whole doc set.

    A set is named by key with no id, which is what makes a set reference: its
    lock has its own version, separate from any member's. A doc is named by
    id.
    """
    namespace: str
    id: str
    version: int
    key: str = ''

    def is_set_ref(self) -> bool:
        """True when this names a whole doc set rather than one doc."""
        return not self.id and bool(self.key)

    def __str__(self) -> str:
        if self.is_set_ref():
            return f'{self.namespace}/[{self.key}]:v{self.version}'
        return f'{self.namespace}/{self.id}:v{self.version}'


@dataclass
class DocClaim:
    """Names one doc set to claim, by namespace and primary key.

    omit_members claims the set and returns none of its docs. Its holder can
    read them later, or insert one it knows is new, without racing anyone.

    duration_s is ignored. A set claimed for a task expires in step with that
    task, from a single reading of the store's clock, so there is no separate
    duration to give. EntroQBase.claim_doc_sets takes duration_ms for a claim
    made without a task to tie it to.
    """
    namespace: str
    key: str
    duration_s: Optional[float] = None
    omit_members: bool = False

    def __post_init__(self) -> None:
        if self.duration_s is not None:
            warnings.warn(
                'DocClaim.duration_s is ignored: a set claimed for a task '
                'expires with that task',
                DeprecationWarning, stacklevel=3)


@dataclass
class DocData:
    """Input spec for a new doc insert."""
    namespace: str
    key: str
    secondary_key: str = ''
    content: Any = None
    id: Optional[str] = None
    at: Optional[datetime] = None


@dataclass
class TaskArrival:
    """Renews or releases a task's claim, changing nothing else about it.

    by_s is how long from the backend's own now the task arrives: positive
    holds it, zero or less releases it. A duration rather than an instant, so
    the arrival does not depend on this process's clock agreeing with the
    store's, nor on how long the request takes to get there.
    """
    id: str
    version: int
    queue: str
    by_s: float = 0.0


@dataclass
class DocArrival:
    """Renews or releases a doc set's claim, naming the set by its key.

    Naming the set rather than its members is what lets a set with no docs be
    renewed at all. by_s is as for TaskArrival.
    """
    namespace: str
    key: str
    version: int
    by_s: float = 0.0


@dataclass
class DocChange:
    """Specifies new values for an existing doc (identified by namespace + id + version).

    at=None releases the claim (snaps to now, clears claimant).
    at=future_datetime renews/sets the claim.
    Keys (key, secondary_key) must match existing values; they are carried along
    but the backend treats them as immutable after creation.
    """
    namespace: str
    id: str
    version: int
    key: str
    secondary_key: str
    content: Any = None
    at: Optional[datetime] = None


@dataclass
class Doc:
    """A complete doc object."""
    namespace: str
    id: str
    version: int
    key: str
    secondary_key: str
    content: Any
    claimant: str = ''
    at: Optional[datetime] = None
    created: Optional[datetime] = None
    modified: Optional[datetime] = None

    def as_id(self) -> 'DocID':
        return DocID(namespace=self.namespace, id=self.id, version=self.version)

    def is_set_ref(self) -> bool:
        """True when this names a whole doc set rather than one doc.

        A set comes back as a doc with no ID, carrying the set's own lock:
        namespace, key, version, claimant, arrival. Renewals and releases name
        sets, so a response to one holds these alongside real docs.
        """
        return not self.id and bool(self.key)

    def as_change(self, **overrides) -> 'DocChange':
        """Return a DocChange for this doc with optional field overrides.

        By default copies existing content and releases the claim (at=None).
        Pass at=future_datetime to renew instead.
        """
        return DocChange(
            namespace=overrides.get('namespace', self.namespace),
            id=overrides.get('id', self.id),
            version=overrides.get('version', self.version),
            key=overrides.get('key', self.key),
            secondary_key=overrides.get('secondary_key', self.secondary_key),
            content=overrides.get('content', self.content),
            at=overrides.get('at', None),
        )


class ClaimedDocs(list):
    """The member docs of claimed sets, with the sets themselves on ``sets``.

    This is a list of the members, so iterating, indexing, or counting a claim
    result works as it always has. ``sets`` holds each claimed set at its own
    lock version, which is what a renewal or release names: a set has a lock
    even with no docs under it, and naming the set is the only way to renew a
    claim on an empty one.
    """

    def __init__(self, docs: Iterable[Doc] = (), sets: Iterable[Doc] = ()) -> None:
        super().__init__(docs)
        self.sets: List[Doc] = list(sets)


class DependencyError(Exception):
    """Raised when a modify call fails due to dependency constraints.

    Task-scoped and doc-scoped failures are kept apart. The ``doc_*`` lists
    hold :class:`DocID` values; the rest hold :class:`TaskID` values. Workers
    use :meth:`has_missing_docs` and :meth:`has_claimed_docs` to tell a poison
    pill (a required doc is gone) from transient contention (another claimant
    holds it), mirroring the Go client.
    """
    def __init__(self, message="", missing=(), mismatched=(), collisions=(), inserts=(), depends=(), deletes=(), changes=(), claims=(),
                 doc_inserts=(), doc_depends=(), doc_deletes=(), doc_changes=(), doc_claims=()):
        super().__init__(message)
        self.message = message
        self.missing = list(missing)
        self.mismatched = list(mismatched)
        self.collisions = list(collisions)
        self.inserts = list(inserts)
        self.depends = list(depends)
        self.deletes = list(deletes)
        self.changes = list(changes)
        self.claims = list(claims)
        self.doc_inserts = list(doc_inserts)
        self.doc_depends = list(doc_depends)
        self.doc_deletes = list(doc_deletes)
        self.doc_changes = list(doc_changes)
        self.doc_claims = list(doc_claims)

    def has_missing_docs(self) -> bool:
        """True when a required doc is absent, not merely claimed elsewhere.

        A task whose required doc no longer exists is a poison pill: retrying
        cannot help.
        """
        return bool(self.doc_depends or self.doc_deletes or self.doc_changes)

    def has_claimed_docs(self) -> bool:
        """True when a required doc is held by another claimant (transient)."""
        return bool(self.doc_claims)

    def __str__(self):
        return json.dumps({
            'message': self.message,
            'missing': [str(t) for t in self.missing],
            'mismatched': [str(t) for t in self.mismatched],
            'collisions': [str(t) for t in self.collisions],
            'inserts': [str(t) for t in self.inserts],
            'depends': [str(t) for t in self.depends],
            'deletes': [str(t) for t in self.deletes],
            'changes': [str(t) for t in self.changes],
            'claims': [str(t) for t in self.claims],
            'docInserts': [str(d) for d in self.doc_inserts],
            'docDepends': [str(d) for d in self.doc_depends],
            'docDeletes': [str(d) for d in self.doc_deletes],
            'docChanges': [str(d) for d in self.doc_changes],
            'docClaims': [str(d) for d in self.doc_claims],
        })


class TransportError(Exception):
    """Raised when a client could not complete an exchange with its backend.

    ``safe_to_retry`` is true only when the client knows the operation was not
    submitted. When it is false, the backend may have committed the operation
    before the connection failed, so callers should not blindly repeat it.
    The original transport exception is available as ``cause`` and as the
    exception's ``__cause__``.
    """

    def __init__(self, message: str, *, cause: Exception, safe_to_retry: bool) -> None:
        super().__init__(message)
        self.cause = cause
        self.safe_to_retry = safe_to_retry


# ---------------------------------------------------------------------------
# Modification and ModifyResult
# ---------------------------------------------------------------------------

class _Op(ABC):
    """Base for atomic modification operations. Internal use only."""
    @abstractmethod
    def _apply(self, m: Modification) -> None: ...


class _TaskInsert(_Op):
    def __init__(self, data: TaskData) -> None:
        self.data = data
    def _apply(self, m: Modification) -> None:
        m.task_inserts.append(self.data)

class _TaskChange(_Op):
    def __init__(self, change: TaskChange) -> None:
        self.change = change
    def _apply(self, m: Modification) -> None:
        m.task_changes.append(self.change)

class _TaskArrival(_Op):
    def __init__(self, arrival: TaskArrival) -> None:
        self.arrival = arrival
    def _apply(self, m: Modification) -> None:
        m.task_arrivals.append(self.arrival)

class _DocArrival(_Op):
    def __init__(self, arrival: DocArrival) -> None:
        self.arrival = arrival
    def _apply(self, m: Modification) -> None:
        m.doc_arrivals.append(self.arrival)

class _TaskDelete(_Op):
    def __init__(self, id: TaskID) -> None:
        self.id = id
    def _apply(self, m: Modification) -> None:
        m.task_deletes.append(self.id)

class _TaskDepend(_Op):
    def __init__(self, id: TaskID) -> None:
        self.id = id
    def _apply(self, m: Modification) -> None:
        m.task_depends.append(self.id)

class _DocInsert(_Op):
    def __init__(self, data: DocData) -> None:
        self.data = data
    def _apply(self, m: Modification) -> None:
        m.doc_inserts.append(self.data)

class _DocChange(_Op):
    def __init__(self, change: DocChange) -> None:
        self.change = change
    def _apply(self, m: Modification) -> None:
        m.doc_changes.append(self.change)

class _DocDelete(_Op):
    def __init__(self, id: DocID) -> None:
        self.id = id
    def _apply(self, m: Modification) -> None:
        m.doc_deletes.append(self.id)

class _DocDepend(_Op):
    def __init__(self, id: DocID) -> None:
        self.id = id
    def _apply(self, m: Modification) -> None:
        m.doc_depends.append(self.id)


class Modification:
    """An atomic set of task and doc operations, built from classmethod factories.

    Example::

        from entroq.types import Modification as M

        return M(
            M.deleting(task),
            M.changing(doc, content=new_content),
            M.inserting(DocData(namespace='/state', key='counter', content=0)),
        )
    """

    def __init__(self, *ops: _Op) -> None:
        self.task_inserts: List[TaskData] = []
        self.task_changes: List[TaskChange] = []
        self.task_deletes: List[TaskID] = []
        self.task_depends: List[TaskID] = []
        self.doc_inserts: List[DocData] = []
        self.doc_changes: List[DocChange] = []
        self.doc_deletes: List[DocID] = []
        self.doc_depends: List[DocID] = []
        self.task_arrivals: List[TaskArrival] = []
        self.doc_arrivals: List[DocArrival] = []
        for op in ops:
            op._apply(self)

    def is_empty(self) -> bool:
        """True when this names no operation at all.

        A backend refuses such a modification: one that does nothing is a
        mistake, most often a list its builder forgot to carry.
        """
        return not (self.task_inserts or self.task_changes or self.task_deletes
                    or self.task_depends or self.task_arrivals
                    or self.doc_inserts or self.doc_changes or self.doc_deletes
                    or self.doc_depends or self.doc_arrivals)

    @classmethod
    def inserting(cls, item: Union[TaskData, DocData]) -> _Op:
        """Return an insert op for a TaskData or DocData."""
        if isinstance(item, TaskData):
            return _TaskInsert(item)
        return _DocInsert(item)

    @classmethod
    def changing(cls, item: Union[Task, TaskChange, Doc, DocChange], **overrides) -> _Op:
        """Return a change op, optionally overriding fields on Task or Doc."""
        if isinstance(item, (Task, TaskChange)):
            return _TaskChange(item.as_change(**overrides) if isinstance(item, Task) else item)
        return _DocChange(item.as_change(**overrides) if isinstance(item, Doc) else item)

    @classmethod
    def arriving(cls, item: Union[Task, Doc], by_s: float) -> _Op:
        """Return an op making a task or a doc's set ready again in by_s.

        It changes only the arrival: a positive by_s renews the claim, and zero
        or less releases it. Nothing else about the item is sent, so a renewal
        cannot carry a stale value back to the store, and does not resend a
        payload every cycle the way a full change does.

        A Doc must be a set reference (see Doc.is_set_ref): an arrival belongs
        to the set, whose lock has its own version, and a member's version is
        a different number that would fail the check or pass it by accident.
        Claiming a set returns it on ClaimedDocs.sets.
        """
        if isinstance(item, Task):
            return _TaskArrival(TaskArrival(item.id, item.version, item.queue, by_s))
        if not item.is_set_ref():
            raise ValueError(
                f'arriving: {item.namespace}/{item.id} is a doc, not a set; '
                'name the set it belongs to, from ClaimedDocs.sets')
        return _DocArrival(DocArrival(item.namespace, item.key, item.version, by_s))

    @classmethod
    def deleting(cls, item: Union[Task, TaskID, Doc, DocID]) -> _Op:
        """Return a delete op for a task or doc."""
        if isinstance(item, (Task, TaskID)):
            return _TaskDelete(item.as_id() if isinstance(item, Task) else item)
        return _DocDelete(item.as_id() if isinstance(item, Doc) else item)

    @classmethod
    def depending(cls, item: Union[Task, TaskID, Doc, DocID]) -> _Op:
        """Return a depend op for a task or doc (version-pins without modifying)."""
        if isinstance(item, (Task, TaskID)):
            return _TaskDepend(item.as_id() if isinstance(item, Task) else item)
        return _DocDepend(item.as_id() if isinstance(item, Doc) else item)


@dataclass
class ModifyResult:
    """Result of a modify_all() call."""
    tasks_inserted: List[Task] = field(default_factory=list)
    tasks_changed: List[Task] = field(default_factory=list)
    docs_inserted: List[Doc] = field(default_factory=list)
    docs_changed: List[Doc] = field(default_factory=list)
