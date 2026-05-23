from abc import ABC, abstractmethod
from datetime import datetime
from typing import List, Tuple, Optional, Union

from .types import Task, TaskData, TaskChange, TaskID, Doc, DocData, DocChange, DocID

class EntroQBase(ABC):
    """Abstract base class for EntroQ clients."""

    @abstractmethod
    def time(self) -> datetime:
        """Return the current time according to the backend."""
        pass

    @abstractmethod
    def queues(self, prefix: str = '', exact: List[str] = (), limit: int = 0) -> List[dict]:
        """Return queue statistics."""
        pass

    @abstractmethod
    def tasks(self, queue: str = '', limit: int = 0, omit_values: bool = False) -> List[Task]:
        """Return a list of tasks in a queue."""
        pass

    @abstractmethod
    def try_claim(self, queue: Union[str, List[str]], duration_ms: int = 30000) -> Optional[Task]:
        """Attempt to claim a task immediately."""
        pass

    @abstractmethod
    def claim(self, queue: Union[str, List[str]], duration_ms: int = 30000, poll_ms: int = 5000, timeout_s: Optional[float] = None) -> Task:
        """Block until a task is available, then claim it."""
        pass

    @abstractmethod
    def modify(
        self,
        inserts: List[TaskData] = (),
        changes: List[TaskChange] = (),
        deletes: List[Union[Task, TaskID]] = (),
        depends: List[Union[Task, TaskID]] = (),
        unsafe_claimant_id: Optional[str] = None,
    ) -> Tuple[List[Task], List[Task]]:
        """Atomically apply task modifications."""
        pass

    @abstractmethod
    def docs(
        self,
        namespace: str = '',
        key_start: str = '',
        key_end: str = '',
        limit: int = 0,
        omit_values: bool = False,
    ) -> List[Doc]:
        """Return docs in a namespace, optionally filtered by key range [key_start, key_end)."""
        pass

    @abstractmethod
    def claim_docs(
        self,
        namespace: str,
        key: str,
        duration_ms: int = 30000,
    ) -> List[Doc]:
        """Atomically claim all docs sharing key in namespace.

        Returns empty list if no docs with that key exist.
        Raises DependencyError if any matching doc is already claimed by another claimant.
        """
        pass

    @abstractmethod
    def modify_docs(
        self,
        inserts: List[DocData] = (),
        changes: List[DocChange] = (),
        deletes: List[Union[Doc, DocID]] = (),
        depends: List[Union[Doc, DocID]] = (),
        unsafe_claimant_id: Optional[str] = None,
    ) -> Tuple[List[Doc], List[Doc]]:
        """Atomically apply doc modifications. Returns (inserted_docs, changed_docs)."""
        pass
