from __future__ import annotations

import asyncio
import secrets
from collections.abc import Sequence
from datetime import datetime, timezone

import httpx

from .types import (
    Task, TaskData, TaskChange, TaskID,
    Doc, DocData, DocChange, DocID,
    DependencyError, Modification, ModifyResult, TransportError,
)
from .base import EntroQBase


def _parse_ms(ms: int | str) -> datetime:
    return datetime.fromtimestamp(int(ms) / 1000.0, tz=timezone.utc)


def _to_ms(dt: datetime | None) -> int:
    return 0 if dt is None else int(dt.timestamp() * 1000)


def _task_from_json(obj: dict) -> Task:
    return Task(
        id=obj.get("id", ""),
        version=int(obj.get("version", 0)),
        queue=obj.get("queue", ""),
        at=_parse_ms(obj.get("atMs", 0)),
        claimant=obj.get("claimantId", ""),
        value=obj.get("value"),
        created=_parse_ms(obj.get("createdMs", 0)),
        modified=_parse_ms(obj.get("modifiedMs", 0)),
        claims=int(obj.get("claims", 0)),
        attempt=int(obj.get("attempt", 0)),
        err=obj.get("err", ""),
    )


def _doc_from_json(obj: dict) -> Doc:
    return Doc(
        namespace=obj.get("namespace", ""),
        id=obj.get("id", ""),
        version=int(obj.get("version", 0)),
        key=obj.get("key", ""),
        secondary_key=obj.get("secondaryKey", ""),
        content=obj.get("content"),
        claimant=obj.get("claimant", ""),
        at=_parse_ms(obj["atMs"]) if obj.get("atMs") else None,
        created=_parse_ms(obj["createdMs"]) if obj.get("createdMs") else None,
        modified=_parse_ms(obj["modifiedMs"]) if obj.get("modifiedMs") else None,
    )


def _task_id_json(t: Task | TaskID) -> dict:
    return {"id": t.id, "version": t.version, "queue": getattr(t, "queue", "")}


def _doc_id_json(d: Doc | DocID) -> dict:
    return {"namespace": d.namespace, "id": d.id, "version": d.version}


def _task_insert_json(i: TaskData) -> dict:
    return {k: v for k, v in {
        "queue": i.queue,
        "atMs": _to_ms(i.at) or None,
        "value": i.value,
        "id": i.id or None,
        "attempt": i.attempt or None,
        "err": i.err or None,
    }.items() if v is not None}


def _task_change_json(c: TaskChange) -> dict:
    return {
        # oldId.queue is the source (from_queue), part of the modify key that the
        # service matches; newData.queue is the destination (equal for no move).
        "oldId": {"id": c.id, "version": c.version, "queue": c.from_queue},
        "newData": {
            "queue": c.queue,
            "atMs": _to_ms(c.at),
            "value": c.value,
            "attempt": c.attempt,
            "err": c.err,
        },
    }


def _doc_insert_json(d: DocData) -> dict:
    return {k: v for k, v in {
        "namespace": d.namespace,
        "id": d.id or None,
        "key": d.key,
        "secondaryKey": d.secondary_key or None,
        "content": d.content,
        "atMs": _to_ms(d.at) or None,
    }.items() if v is not None}


def _doc_change_json(c: DocChange) -> dict:
    return {
        "oldId": {"namespace": c.namespace, "id": c.id, "version": c.version},
        "newData": {
            "namespace": c.namespace,
            "key": c.key,
            "secondaryKey": c.secondary_key,
            "content": c.content,
            "atMs": _to_ms(c.at),
        },
    }


class EntroQJSON(EntroQBase):
    """EntroQ client talking to the REST/gRPC-gateway API at /api/v0."""

    def __init__(
        self,
        base_url: str,
        claimant_id: str | None = None,
        *,
        http_client: httpx.AsyncClient | None = None,
    ) -> None:
        """Create a JSON client.

        The default HTTP client has no transport deadlines: EntroQ claims are
        intentionally long-held requests, and mutating requests must not be
        abandoned merely because a response takes five seconds. Caller task
        cancellation remains effective. Connection counts retain httpx's bounded
        defaults, while idle connections live long enough to span the default
        task-renewal interval.

        An injected ``http_client`` is public and caller-owned; :meth:`aclose`
        leaves it open. Otherwise ``http`` is created and owned by this client.
        """
        self._base_url = base_url.rstrip("/")
        self.claimant_id = claimant_id or secrets.token_hex(8)
        self._owns_http = http_client is None
        self.http = http_client or httpx.AsyncClient(
            timeout=None,
            limits=httpx.Limits(
                max_connections=100,
                max_keepalive_connections=20,
                keepalive_expiry=60.0,
            ),
        )
        # Preserve the former private escape hatch while callers migrate to the
        # supported public attribute.
        self._http = self.http

    async def aclose(self) -> None:
        """Close the HTTP client created by this EntroQ client, if any."""
        if self._owns_http:
            await self.http.aclose()

    async def _request(self, method: str, path: str, *, json=None, params=None) -> dict:
        try:
            resp = await self.http.request(method, f"{self._base_url}{path}", json=json, params=params)
        except httpx.TransportError as e:
            # Connect and pool failures happen before an HTTP request reaches the
            # service. Read/write/protocol failures are ambiguous: the service may
            # already have committed a claim or modification.
            safe_to_retry = isinstance(e, (httpx.ConnectError, httpx.ConnectTimeout, httpx.PoolTimeout))
            raise TransportError(
                f"{method} {path}: {e}",
                cause=e,
                safe_to_retry=safe_to_retry,
            ) from e
        if not resp.is_success:
            self._raise_for_error(resp)
        if resp.status_code == 204:
            return {}
        return resp.json()

    def _raise_for_error(self, resp: httpx.Response) -> None:
        # Dependency errors arrive as 409 Conflict (Aborted). 404 is also
        # accepted for tolerance; the dependency-detail check below keeps an
        # ordinary 404 from being misread as a dependency error.
        if resp.status_code in (409, 404):
            try:
                body = resp.json()
                details = body.get("details", [])
                _DEP_TYPES = {"INSERT", "CHANGE", "DELETE", "DEPEND", "CLAIM", "DETAIL"}
                if any(d.get("type") in _DEP_TYPES for d in details):
                    kwargs: dict = {"message": body.get("message", "")}
                    # A ModifyDep carries either a task id or a doc_id, never
                    # both. Reading only "id" collapses every doc dependency to
                    # None, which is what made doc failures uninspectable.
                    _TASK_KEY = {
                        "INSERT": "inserts", "CHANGE": "changes", "DELETE": "deletes",
                        "DEPEND": "depends", "CLAIM": "claims",
                    }
                    _DOC_KEY = {
                        "INSERT": "doc_inserts", "CHANGE": "doc_changes",
                        "DELETE": "doc_deletes", "DEPEND": "doc_depends",
                        "CLAIM": "doc_claims",
                    }
                    for d in details:
                        dtype = d.get("type")
                        if dtype == "DETAIL":
                            kwargs["message"] = d.get("msg", kwargs["message"])
                            continue
                        did_raw = d.get("docId")
                        if did_raw:
                            key = _DOC_KEY.get(dtype)
                            if key:
                                kwargs.setdefault(key, []).append(DocID(
                                    namespace=did_raw.get("namespace", ""),
                                    id=did_raw["id"],
                                    version=int(did_raw.get("version", 0)),
                                ))
                            continue
                        tid_raw = d.get("id")
                        if not tid_raw:
                            continue
                        key = _TASK_KEY.get(dtype)
                        if key:
                            kwargs.setdefault(key, []).append(TaskID(
                                id=tid_raw["id"],
                                version=int(tid_raw.get("version", 0)),
                                queue=tid_raw.get("queue", ""),
                            ))
                    kwargs["missing"] = kwargs.get("depends", []) + kwargs.get("deletes", [])
                    kwargs["collisions"] = kwargs.get("inserts", [])
                    raise DependencyError(**kwargs)
            except (ValueError, KeyError):
                pass
        resp.raise_for_status()

    async def time(self) -> datetime:
        data = await self._request("GET", "/api/v0/time")
        return _parse_ms(data.get("timeMs", 0))

    async def queues(self, prefix: str = "", exact=(), limit: int = 0) -> list[dict]:
        params: dict = {}
        if prefix:    params["matchPrefix"] = prefix
        if exact:     params["matchExact"] = list(exact)
        if limit:     params["limit"] = limit
        data = await self._request("GET", "/api/v0/queues", params=params)
        return [
            {
                "name": q.get("name", ""),
                "num_tasks": q.get("numTasks", 0),
                "num_claimed": q.get("numClaimed", 0),
                "num_available": q.get("numAvailable", 0),
                "num_future": q.get("numFuture", 0),
            }
            for q in data.get("queues", [])
        ]

    async def tasks(self, queue: str = "", limit: int = 0, omit_values: bool = False) -> list[Task]:
        params: dict = {}
        if queue:        params["queue"] = queue
        if limit:        params["limit"] = limit
        if omit_values:  params["omitValues"] = "true"
        data = await self._request("GET", "/api/v0/tasks", params=params)
        return [_task_from_json(t) for t in data.get("tasks", [])]

    async def try_claim(self, queue: str | list[str], duration_ms: int = 30000) -> Task | None:
        queues = [queue] if isinstance(queue, str) else list(queue)
        data = await self._request("POST", "/api/v0/claim", json={
            "claimantId": self.claimant_id,
            "queues": queues,
            "durationMs": str(duration_ms),
            "pollMs": "0",
        })
        # The server may emit an explicit "task": null when nothing is available
        # (zero-valued fields are not omitted), so check the value, not the key.
        return _task_from_json(data["task"]) if data.get("task") is not None else None

    async def claim(self, queue: str | list[str], duration_ms: int = 30000, poll_ms: int = 30000, timeout_s: float | None = None) -> Task:
        queues = [queue] if isinstance(queue, str) else list(queue)
        async def wait() -> dict:
            return await self._request("POST", "/api/v0/claim/wait", json={
                "claimantId": self.claimant_id,
                "queues": queues,
                "durationMs": str(duration_ms),
                "pollMs": str(poll_ms),
            })

        if timeout_s is None:
            data = await wait()
        else:
            try:
                data = await asyncio.wait_for(wait(), timeout=timeout_s)
            except asyncio.TimeoutError as e:
                raise TimeoutError(f"claim timed out after {timeout_s}s") from e
        if data.get("task") is not None:
            return _task_from_json(data["task"])
        raise RuntimeError("blocking claim returned without a task")

    async def modify(self, modification: Modification, *, unsafe_claimant_id: str | None = None) -> ModifyResult:
        data = await self._request("POST", "/api/v0/modify", json={
            "claimantId": unsafe_claimant_id or self.claimant_id,
            "inserts":    [_task_insert_json(i) for i in modification.task_inserts],
            "changes":    [_task_change_json(c) for c in modification.task_changes],
            "deletes":    [_task_id_json(d) for d in modification.task_deletes],
            "depends":    [_task_id_json(d) for d in modification.task_depends],
            "docInserts": [_doc_insert_json(i) for i in modification.doc_inserts],
            "docChanges": [_doc_change_json(c) for c in modification.doc_changes],
            "docDeletes": [_doc_id_json(d) for d in modification.doc_deletes],
            "docDepends": [_doc_id_json(d) for d in modification.doc_depends],
        })
        return ModifyResult(
            tasks_inserted=[_task_from_json(t) for t in data.get("inserted", [])],
            tasks_changed=[_task_from_json(t) for t in data.get("changed", [])],
            docs_inserted=[_doc_from_json(d) for d in data.get("insertedDocs", [])],
            docs_changed=[_doc_from_json(d) for d in data.get("changedDocs", [])],
        )

    async def docs(
        self,
        namespace: str = "",
        key_start: str = "",
        key_end: str = "",
        limit: int = 0,
        omit_values: bool = False,
        key_exact: str = "",
        ids: Sequence[str] = (),
    ) -> list[Doc]:
        # Docs takes a nested DocQuery, so every filter is transcoded under the
        # "query." field path. Unprefixed names are rejected as unknown fields.
        params: dict = {}
        if namespace:   params["query.namespace"] = namespace
        if key_start:   params["query.keyStart"] = key_start
        if key_end:     params["query.keyEnd"] = key_end
        if limit:       params["query.limit"] = limit
        if omit_values: params["query.omitValues"] = "true"
        if key_exact:   params["query.keyExact"] = key_exact
        if ids:         params["query.ids"] = list(ids)
        data = await self._request("GET", "/api/v0/docs", params=params)
        return [_doc_from_json(d) for d in data.get("docs", [])]

    async def claim_docs(self, namespace: str, key: str, duration_ms: int = 30000) -> list[Doc]:
        data = await self._request("POST", "/api/v0/docs/claim", json={
            "claimQuery": {
                "namespace": namespace,
                "claimant": self.claimant_id,
                "key": key,
                "durationMs": duration_ms,
            },
        })
        return [_doc_from_json(d) for d in data.get("docs", [])]
