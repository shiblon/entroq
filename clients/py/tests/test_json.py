import asyncio
import json
from datetime import datetime, timezone

import httpx
import pytest

from entroq.json import EntroQJSON, _doc_insert_json
from entroq.types import DocData, TransportError


async def _docs_query(**kwargs) -> httpx.QueryParams:
    """Return the query parameters docs(**kwargs) puts on the wire."""
    seen: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(200, json={"docs": []})

    http = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    try:
        eq = EntroQJSON("http://entroq.example", http_client=http)
        await eq.docs(**kwargs)
    finally:
        await http.aclose()
    return seen[0].url.params


async def test_docs_sends_no_params_when_unfiltered():
    """An unfiltered listing must send nothing, not an empty namespace."""
    assert await _docs_query() == httpx.QueryParams()


async def test_docs_prefixes_range_filters_with_query_path():
    params = await _docs_query(
        namespace="status", key_start="a", key_end="b", limit=5, omit_values=True,
    )

    assert dict(params) == {
        "query.namespace": "status",
        "query.keyStart": "a",
        "query.keyEnd": "b",
        "query.limit": "5",
        "query.omitValues": "true",
    }


async def test_docs_prefixes_key_exact():
    params = await _docs_query(namespace="status", key_exact="sess-1")

    assert dict(params) == {"query.namespace": "status", "query.keyExact": "sess-1"}


async def test_docs_repeats_ids_under_one_prefixed_name():
    params = await _docs_query(namespace="status", ids=["id-1", "id-2"])

    assert params.get_list("query.ids") == ["id-1", "id-2"]


async def test_aclose_closes_http_client():
    eq = EntroQJSON("http://localhost")
    assert eq.http is eq._http
    assert not eq.http.is_closed
    assert eq.http.timeout.connect is None
    assert eq.http.timeout.read is None
    assert eq.http.timeout.write is None
    assert eq.http.timeout.pool is None

    await eq.aclose()

    assert eq.http.is_closed


async def test_async_context_manager_closes_http_client():
    async with EntroQJSON("http://localhost") as eq:
        http = eq.http
        assert not http.is_closed

    assert http.is_closed


async def test_injected_http_client_is_public_and_caller_owned():
    http = httpx.AsyncClient(transport=httpx.MockTransport(lambda _: httpx.Response(204)))

    async with EntroQJSON("http://localhost", http_client=http) as eq:
        assert eq.http is http

    assert not http.is_closed
    await http.aclose()


async def test_claim_uses_go_poll_default():
    requests = []

    async def handle(request):
        requests.append(request)
        return httpx.Response(200, json={
            "task": {"id": "t", "queue": "q", "atMs": "0", "createdMs": "0", "modifiedMs": "0"},
        })

    http = httpx.AsyncClient(transport=httpx.MockTransport(handle))
    try:
        eq = EntroQJSON("http://localhost", http_client=http)
        task = await eq.claim("q")
    finally:
        await http.aclose()

    assert task.id == "t"
    assert json.loads(requests[0].content)["pollMs"] == "30000"


async def test_claim_can_be_canceled_by_caller():
    started = asyncio.Event()
    blocked = asyncio.Event()

    async def handle(_):
        started.set()
        await blocked.wait()
        raise AssertionError("canceled request continued")

    http = httpx.AsyncClient(transport=httpx.MockTransport(handle))
    try:
        eq = EntroQJSON("http://localhost", http_client=http)
        claim = asyncio.create_task(eq.claim("q"))
        await started.wait()
        claim.cancel()
        with pytest.raises(asyncio.CancelledError):
            await claim
    finally:
        await http.aclose()


async def test_claim_optional_timeout_cancels_wait():
    async def handle(_):
        await asyncio.Event().wait()

    http = httpx.AsyncClient(transport=httpx.MockTransport(handle))
    try:
        eq = EntroQJSON("http://localhost", http_client=http)
        with pytest.raises(TimeoutError, match="after 0.01s"):
            await eq.claim("q", timeout_s=0.01)
    finally:
        await http.aclose()


@pytest.mark.parametrize(
    ("error_type", "safe_to_retry"),
    [
        (httpx.ConnectError, True),
        (httpx.ReadError, False),
    ],
)
async def test_transport_errors_preserve_retry_safety(error_type, safe_to_retry):
    async def handle(request):
        raise error_type("broken", request=request)

    http = httpx.AsyncClient(transport=httpx.MockTransport(handle))
    try:
        eq = EntroQJSON("http://localhost", http_client=http)
        with pytest.raises(TransportError) as raised:
            await eq.time()
    finally:
        await http.aclose()

    assert raised.value.safe_to_retry is safe_to_retry
    assert isinstance(raised.value.cause, error_type)
    assert raised.value.__cause__ is raised.value.cause


def test_doc_insert_encodes_future_arrival():
    at = datetime(2030, 1, 2, 3, 4, 5, tzinfo=timezone.utc)

    assert _doc_insert_json(DocData(namespace="status", key="session", at=at))["atMs"] == 1893553445000
