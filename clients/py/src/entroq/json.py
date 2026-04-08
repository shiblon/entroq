import requests
import secrets
from datetime import datetime, timezone, timedelta
from typing import List, Tuple, Optional, Union

from .types import Task, TaskData, TaskChange, TaskID, DependencyError
from .base import EntroQBase

def _parse_time(ms_str: str) -> datetime:
    """Parse a millisecond string into a UTC datetime."""
    return datetime.fromtimestamp(int(ms_str) / 1000.0, tz=timezone.utc)

def _to_ms_str(dt: Optional[datetime]) -> str:
    """Convert a datetime to a millisecond string."""
    if dt is None:
        return "0"
    return str(int(dt.timestamp() * 1000))

def _json_to_task(obj: dict) -> Task:
    """Convert a JSON object from the API into a Task dataclass."""
    return Task(
        id=obj.get("id", ""),
        version=int(obj.get("version", 0)),
        queue=obj.get("queue", ""),
        at=_parse_time(obj.get("atMs", "0")),
        claimant=obj.get("claimantId", ""),
        value=obj.get("value"),
        created=_parse_time(obj.get("createdMs", "0")),
        modified=_parse_time(obj.get("modifiedMs", "0")),
        claims=int(obj.get("claims", 0)),
        attempt=int(obj.get("attempt", 0)),
        err=obj.get("err", ""),
    )

class EntroQJSON(EntroQBase):
    """EntroQ client that talks to the RESTful API /api/v0."""

    def __init__(self, base_url: str, claimant_id: str = None):
        self.base_url = base_url.rstrip('/')
        self.claimant_id = claimant_id or secrets.token_hex(8)
        self.session = requests.Session()

    def _request(self, method: str, path: str, json_data=None, params=None):
        url = f"{self.base_url}{path}"
        resp = self.session.request(method, url, json=json_data, params=params)
        
        if not resp.ok:
            try:
                err_detail = resp.json()
                # Vanguard transcode might wrap errors or return them in a specific field.
                # We look for dependency error markers.
                msg = str(err_detail).lower()
                if (resp.status_code == 404 and ("missing" in msg or "mismatched" in msg)) or \
                   (resp.status_code == 500 and "modification dependency error" in msg):
                    kwargs = {'message': err_detail.get('message', '')}
                    for d in err_detail.get('details', []):
                        dtype = d.get('type')
                        tid_raw = d.get('id')
                        tid = None
                        if tid_raw:
                            tid = TaskID(id=tid_raw.get('id'), version=int(tid_raw.get('version', 0)), queue=tid_raw.get('queue', ''))
                        
                        if dtype == 'INSERT': kwargs.setdefault('inserts', []).append(tid)
                        elif dtype == 'CHANGE': kwargs.setdefault('changes', []).append(tid)
                        elif dtype == 'DELETE': kwargs.setdefault('deletes', []).append(tid)
                        elif dtype == 'DEPEND': kwargs.setdefault('depends', []).append(tid)
                        elif dtype == 'CLAIM': kwargs.setdefault('claims', []).append(tid)
                        elif dtype == 'DETAIL': kwargs['message'] = d.get('msg', kwargs['message'])
                    
                    # Backwards compatibility Mapping
                    kwargs['missing'] = kwargs.get('depends', []) + kwargs.get('deletes', [])
                    kwargs['collisions'] = kwargs.get('inserts', [])
                    
                    raise DependencyError(**kwargs)
            except (ValueError, KeyError):
                pass
            resp.raise_for_status()
        
        if resp.status_code == 204:
            return None
        return resp.json()

    def time(self) -> datetime:
        data = self._request("GET", "/api/v0/time")
        return _parse_time(data.get("timeMs", "0"))

    def queues(self, prefix: str = '', exact: List[str] = (), limit: int = 0) -> List[dict]:
        params = {}
        if prefix: params['matchPrefix'] = prefix
        if exact: params['matchExact'] = list(exact)
        if limit: params['limit'] = limit
        data = self._request("GET", "/api/v0/queues", params=params)
        # Normalize: API returns numTasks (camelCase), PG client returns num_tasks.
        return [
            {
                "name": q.get("name", ""),
                "num_tasks": q.get("numTasks", 0),
                "num_claimed": q.get("numClaimed", 0),
                "num_available": q.get("numAvailable", 0),
                "num_future": q.get("numFuture", 0)
            }
            for q in data.get("queues", [])
        ]

    def tasks(self, queue: str = '', limit: int = 0, omit_values: bool = False) -> List[Task]:
        params = {}
        if queue: params['queue'] = queue
        if limit: params['limit'] = limit
        if omit_values: params['omitValues'] = 'true'
        path = "/api/v0/tasks"
        data = self._request("GET", path, params=params)
        return [_json_to_task(t) for t in data.get("tasks", [])]

    def try_claim(self, queue: Union[str, List[str]], duration_ms: int = 30000) -> Optional[Task]:
        queues = [queue] if isinstance(queue, str) else list(queue)
        body = {
            "claimantId": self.claimant_id,
            "queues": queues,
            "durationMs": str(duration_ms),
            "pollMs": "0"
        }
        data = self._request("POST", "/api/v0/claim", json_data=body)
        if data and "task" in data:
            return _json_to_task(data["task"])
        return None

    def claim(self, queue: Union[str, List[str]], duration_ms: int = 30000, poll_ms: int = 5000, timeout_s: Optional[float] = None) -> Task:
        # Note: timeout_s is a client-side timeout for the request.
        # The backend pollMs handles the server-side wait.
        queues = [queue] if isinstance(queue, str) else list(queue)
        body = {
            "claimantId": self.claimant_id,
            "queues": queues,
            "durationMs": str(duration_ms),
            "pollMs": str(poll_ms)
        }
        # For blocking claim, we call the /wait endpoint if available or just /claim.
        # In our proto, we mapped /api/v0/claim/wait to the blocking version.
        data = self._request("POST", "/api/v0/claim/wait", json_data=body)
        if data and "task" in data:
            return _json_to_task(data["task"])
        raise TimeoutError("Claim timed out")

    def modify(
        self,
        inserts: List[TaskData] = (),
        changes: List[TaskChange] = (),
        deletes: List[Union[Task, TaskID]] = (),
        depends: List[Union[Task, TaskID]] = (),
        unsafe_claimant_id: Optional[str] = None,
    ) -> Tuple[List[Task], List[Task]]:
        
        def to_id_obj(it):
            return {"id": it.id, "version": it.version, "queue": getattr(it, 'queue', '')}

        body = {
            "claimantId": unsafe_claimant_id or self.claimant_id,
            "inserts": [
                {k: v for k, v in {
                    "queue": i.queue,
                    "atMs": _to_ms_str(i.at),
                    "value": i.value,
                    "id": i.id or None,
                    "attempt": i.attempt or None,
                    "err": i.err or None,
                }.items() if v is not None} for i in inserts
            ],
            "changes": [
                {
                    "oldId": to_id_obj(c),
                    "newData": {
                        "queue": c.queue,
                        "atMs": _to_ms_str(c.at),
                        "value": c.value,
                        "attempt": c.attempt,
                        "err": c.err
                    }
                } for c in changes
            ],
            "deletes": [to_id_obj(d) for d in deletes],
            "depends": [to_id_obj(d) for d in depends]
        }
        
        data = self._request("POST", "/api/v0/modify", json_data=body)
        inserted = [_json_to_task(t) for t in data.get("inserted", [])]
        changed = [_json_to_task(t) for t in data.get("changed", [])]
        return inserted, changed
