from abc import ABC, abstractmethod
from datetime import datetime
from types import TracebackType
from typing import List, Optional, Sequence, TypeVar, Union

from .types import Task, Doc, Modification, ModifyResult


_EntroQ = TypeVar('_EntroQ', bound='EntroQBase')


class EntroQBase(ABC):
    """Abstract base class for EntroQ clients. All methods are async."""

    async def aclose(self) -> None:
        """Release resources owned by the client.

        The default implementation is a no-op for clients that do not retain
        resources between operations.
        """
        return None

    async def __aenter__(self: _EntroQ) -> _EntroQ:
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc_value: BaseException | None,
        traceback: TracebackType | None,
    ) -> None:
        await self.aclose()

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
    async def claim(self, queue: Union[str, List[str]], duration_ms: int = 30000, poll_ms: int = 30000, timeout_s: Optional[float] = None) -> Task:
        """Block until a task is available, then claim it.

        The wait is indefinite unless ``timeout_s`` is supplied or the calling
        asyncio task is canceled.
        """

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
        key_exact: str = '',
        ids: Sequence[str] = (),
    ) -> List[Doc]:
        """Return docs in a namespace, filtered by one of three exclusive modes.

        The filter modes are mutually exclusive and applied in this precedence:

        - ``ids``: return only those docs. Key range and limit are ignored.
        - ``key_exact``: return every doc whose primary key is exactly this.
        - ``key_start``/``key_end``: half-open range ``[key_start, key_end)`` on
          the primary key. An empty ``key_end`` lists from ``key_start`` onward.

        Passing no filter lists the whole namespace. ``key_exact`` is the way to
        read one document's group without claiming it.
        """

    @abstractmethod
    async def claim_docs(
        self,
        namespace: str,
        key: str,
        duration_ms: int = 30000,
    ) -> List[Doc]:
        """Atomically claim all docs sharing key in namespace."""
