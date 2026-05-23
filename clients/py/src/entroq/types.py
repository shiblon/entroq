import json
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Optional, List, Tuple, Any

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
    id: Optional[str] = None        # None -> auto-generate UUID

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
