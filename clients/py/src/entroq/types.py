import json
from dataclasses import dataclass
from datetime import datetime
from typing import Optional, List, Tuple

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
    value: bytes = b''
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
    value: bytes = b''
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
    value: bytes
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
