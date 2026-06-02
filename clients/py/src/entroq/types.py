from __future__ import annotations

import json
from abc import ABC, abstractmethod
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
    """Specifies new values for an existing task (identified by id + version)."""
    id: str
    version: int
    queue: str
    at: Optional[datetime]          # required - set explicitly or copy from Task
    value: Any = None
    attempt: int = 0
    err: str = ''

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
        """Return a TaskChange for this task with optional field overrides."""
        return TaskChange(
            id=self.id,
            version=self.version,
            queue=overrides.get('queue', self.queue),
            at=overrides.get('at', self.at),
            value=overrides.get('value', self.value),
            attempt=overrides.get('attempt', self.attempt),
            err=overrides.get('err', self.err),
        )

@dataclass
class DocID:
    """Identifies a specific version of a doc (for deletes and depends)."""
    namespace: str
    id: str
    version: int


@dataclass
class DocData:
    """Input spec for a new doc insert."""
    namespace: str
    key: str
    secondary_key: str = ''
    content: Any = None
    id: Optional[str] = None


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


class DependencyError(Exception):
    """Raised when a modify call fails due to dependency constraints."""
    def __init__(self, message="", missing=(), mismatched=(), collisions=(), inserts=(), depends=(), deletes=(), changes=(), claims=()):
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
        })


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
        for op in ops:
            op._apply(self)

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
