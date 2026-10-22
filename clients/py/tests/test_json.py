import asyncio
import json
from datetime import datetime, timedelta, timezone

import httpx
import pytest

from entroq.json import EntroQJSON, _doc_insert_json
from entroq.types import (
    DependencyError, DocData, DocID, InvalidArgumentError, Modification, TaskID,
    TransportError,
)


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


async def _modify_dep_error(details: list[dict]) -> DependencyError:
    """Return the DependencyError the client decodes from these wire details."""
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(409, json={"message": "dependency", "details": details})

    http = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    try:
        eq = EntroQJSON("http://entroq.example", http_client=http)
        with pytest.raises(DependencyError) as raised:
            await eq.modify(Modification())
        return raised.value
    finally:
        await http.aclose()


async def test_dependency_error_decodes_doc_ids():
    """A ModifyDep carries `docId` for docs; reading only `id` loses them."""
    err = await _modify_dep_error([
        {"type": "DEPEND", "docId": {"namespace": "ns", "id": "d1", "version": 3}},
        {"type": "CLAIM", "docId": {"namespace": "ns", "id": "d2", "version": 4}},
    ])
    assert err.doc_depends == [DocID(namespace="ns", id="d1", version=3)]
    assert err.doc_claims == [DocID(namespace="ns", id="d2", version=4)]
    assert err.has_missing_docs()
    assert err.has_claimed_docs()
    # Doc details must not leak into the task-scoped buckets as None padding.
    assert err.depends == []
    assert err.claims == []
    assert err.missing == []


async def test_dependency_error_decodes_a_contended_doc_set():
    """A set is named by key with no `id`, and must still decode as a claim.

    This is the shape a contended set claim really sends. Requiring an `id`
    raises past the decoder and the caller sees an HTTP error instead, which
    costs the worker its one way to tell contention from a poison pill.
    """
    err = await _modify_dep_error([
        {"type": "DETAIL", "msg": 'doc set "cfg" is claimed by someone else'},
        {"type": "CLAIM", "docId": {"namespace": "ns", "key": "cfg", "version": 1}},
        {"type": "CLAIM", "docId": {"namespace": "ns", "id": "d1", "version": 1}},
    ])
    assert err.doc_claims == [
        DocID(namespace="ns", id="", version=1, key="cfg"),
        DocID(namespace="ns", id="d1", version=1),
    ]
    assert err.doc_claims[0].is_set_ref()
    assert not err.doc_claims[1].is_set_ref()
    assert err.has_claimed_docs(), "contention, so the worker backs off"
    assert not err.has_missing_docs(), "not a poison pill"
    assert "claimed by someone else" in str(err)
    assert "ns/[cfg]:v1" in str(err), "a set must print as a set"


async def test_dependency_error_keeps_task_ids_separate():
    err = await _modify_dep_error([
        {"type": "CLAIM", "id": {"id": "t1", "version": 2, "queue": "q"}},
    ])
    assert err.claims == [TaskID(id="t1", version=2, queue="q")]
    assert err.doc_claims == []
    assert not err.has_claimed_docs()
    assert not err.has_missing_docs()


async def test_dependency_error_contended_doc_is_not_missing():
    """Contention must not read as a poison pill: the dispositions differ."""
    err = await _modify_dep_error([
        {"type": "CLAIM", "docId": {"namespace": "ns", "id": "d1", "version": 1}},
    ])
    assert err.has_claimed_docs()
    assert not err.has_missing_docs()


def test_doc_insert_encodes_an_arrival_as_a_duration():
    """An arrival goes out as a duration, never as an instant.

    The service resolves a duration against its own clock, so the offset between
    the two clocks cancels. An instant does not cancel -- it is wrong by that
    offset, and it can also go stale in flight, where a renewal that merely
    arrived late reads as a deliberate release. The conversion happens in the
    client because only this process knows its own now; the service converting an
    instant would be using the wrong clock to do it.
    """
    at = datetime.now(tz=timezone.utc) + timedelta(minutes=5)

    got = _doc_insert_json(DocData(namespace="status", key="session", at=at))

    assert "atMs" not in got, "an instant must not ride along"
    assert abs(got["byMs"] - 5 * 60 * 1000) < 2000, got["byMs"]


def test_doc_insert_with_no_arrival_sends_no_duration():
    """No arrival means ready now, which the wire says by omission."""
    got = _doc_insert_json(DocData(namespace="status", key="session"))

    assert "byMs" not in got and "atMs" not in got, got


def test_doc_insert_encodes_a_past_arrival_as_a_negative_duration():
    """A past instant is a negative duration, which means the same thing.

    Zero and absent both mean now, so a negative value is the only way to say
    "already past" distinctly -- and it survives the omit-falsy encoding that
    drops a zero.
    """
    at = datetime.now(tz=timezone.utc) - timedelta(minutes=5)

    got = _doc_insert_json(DocData(namespace="status", key="session", at=at))

    assert got["byMs"] < 0, got


async def _refused(**response) -> InvalidArgumentError:
    """Return the error the client raises for a 400 shaped like this."""
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(400, **response)

    http = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    try:
        eq = EntroQJSON("http://entroq.example", http_client=http)
        with pytest.raises(InvalidArgumentError) as raised:
            await eq.docs(namespace="ns")
        return raised.value
    finally:
        await http.aclose()


async def test_a_refused_request_carries_the_services_own_reason():
    """A 400 is the service saying the request was wrong, so say what it said.

    Leaving it as the transport's exception would make a caller import httpx to
    tell a malformed request from a lost connection.
    """
    err = await _refused(json={"code": 3, "message": "docs: docs query must name a namespace"})
    assert "must name a namespace" in str(err)


async def test_a_refused_request_without_a_reason_falls_back_to_the_status():
    """A body that carries no message still has to produce a usable error."""
    err = await _refused(text="not json at all")
    assert "400" in str(err)
