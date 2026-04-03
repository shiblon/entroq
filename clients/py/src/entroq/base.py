from abc import ABC, abstractmethod
from datetime import datetime
from typing import List, Tuple, Optional, Union

from .types import Task, TaskData, TaskChange, TaskID

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
        """Atomically apply modifications."""
        pass
