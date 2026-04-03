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
    def __init__(self, missing=(), mismatched=(), collisions=()):
        self.missing = list(missing)
        self.mismatched = list(mismatched)
        self.collisions = list(collisions)

    def __str__(self):
        return json.dumps({
            'missing': self.missing,
            'mismatched': self.mismatched,
            'collisions': self.collisions,
        })
