from abc import ABC, abstractmethod
from datetime import datetime
from typing import List, Optional, Sequence, Union

from .types import Task, Doc, Modification, ModifyResult


class EntroQBase(ABC):
    """Abstract base class for EntroQ clients. All methods are async."""

    @abstractmethod
    async def time(self) -> datetime:
        """Return the current time according to the backend."""

    @abstractmethod
    async def queues(self, prefix: str = '', exact: Sequence[str] = (), limit: int = 0) -> List[dict]:
        """Return queue statistics."""

    @abstractmethod
    async def tasks(self, queue: str = '', limit: int = 0, omit_values: bool = False) -> List[Task]:
        """Return a list of tasks in a queue."""

    @abstractmethod
    async def try_claim(self, queue: Union[str, List[str]], duration_ms: int = 30000) -> Optional[Task]:
        """Attempt to claim a task; returns None immediately if none available."""

    @abstractmethod
    async def claim(self, queue: Union[str, List[str]], duration_ms: int = 30000, poll_ms: int = 5000, timeout_s: Optional[float] = None) -> Task:
        """Block until a task is available, then claim it."""

    @abstractmethod
    async def modify(self, modification: Modification, *, unsafe_claimant_id: str | None = None) -> ModifyResult:
        """Atomically apply task and doc modifications in a single operation."""

    @abstractmethod
    async def docs(
        self,
        namespace: str = '',
        key_start: str = '',
        key_end: str = '',
        limit: int = 0,
        omit_values: bool = False,
    ) -> List[Doc]:
        """Return docs in a namespace, optionally filtered by key range [key_start, key_end)."""

    @abstractmethod
    async def claim_docs(
        self,
        namespace: str,
        key: str,
        duration_ms: int = 30000,
    ) -> List[Doc]:
        """Atomically claim all docs sharing key in namespace."""
