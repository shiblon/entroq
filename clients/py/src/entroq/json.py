from __future__ import annotations

import secrets
from datetime import datetime, timezone

import httpx

from .types import (
    Task, TaskData, TaskChange, TaskID,
    Doc, DocData, DocChange, DocID,
    DependencyError, Modification, ModifyResult,
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
        "oldId": {"id": c.id, "version": c.version, "queue": c.queue},
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

    def __init__(self, base_url: str, claimant_id: str | None = None) -> None:
        self._base_url = base_url.rstrip("/")
        self.claimant_id = claimant_id or secrets.token_hex(8)
        self._http = httpx.AsyncClient()

    async def _request(self, method: str, path: str, *, json=None, params=None) -> dict:
        resp = await self._http.request(method, f"{self._base_url}{path}", json=json, params=params)
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
                    for d in details:
                        dtype = d.get("type")
                        tid_raw = d.get("id")
                        tid = (
                            TaskID(id=tid_raw["id"], version=int(tid_raw.get("version", 0)), queue=tid_raw.get("queue", ""))
                            if tid_raw else None
                        )
                        if dtype == "INSERT":   kwargs.setdefault("inserts", []).append(tid)
                        elif dtype == "CHANGE": kwargs.setdefault("changes", []).append(tid)
                        elif dtype == "DELETE": kwargs.setdefault("deletes", []).append(tid)
                        elif dtype == "DEPEND": kwargs.setdefault("depends", []).append(tid)
                        elif dtype == "CLAIM":  kwargs.setdefault("claims", []).append(tid)
                        elif dtype == "DETAIL": kwargs["message"] = d.get("msg", kwargs["message"])
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

    async def claim(self, queue: str | list[str], duration_ms: int = 30000, poll_ms: int = 5000, timeout_s: float | None = None) -> Task:
        queues = [queue] if isinstance(queue, str) else list(queue)
        data = await self._request("POST", "/api/v0/claim/wait", json={
            "claimantId": self.claimant_id,
            "queues": queues,
            "durationMs": str(duration_ms),
            "pollMs": str(poll_ms),
        })
        if data.get("task") is not None:
            return _task_from_json(data["task"])
        raise TimeoutError("claim timed out")

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
    ) -> list[Doc]:
        params: dict = {"namespace": namespace}
        if key_start:   params["keyStart"] = key_start
        if key_end:     params["keyEnd"] = key_end
        if limit:       params["limit"] = limit
        if omit_values: params["omitValues"] = "true"
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
