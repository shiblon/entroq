import httpx

from entroq.json import EntroQJSON


async def _docs_query(**kwargs) -> httpx.QueryParams:
    """Return the query parameters docs(**kwargs) puts on the wire."""
    seen: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(200, json={"docs": []})

    eq = EntroQJSON("http://entroq.example")
    await eq.aclose()  # discard the real pool; swap in the mock transport.
    eq._http = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    try:
        await eq.docs(**kwargs)
    finally:
        await eq.aclose()
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
    assert not eq._http.is_closed

    await eq.aclose()

    assert eq._http.is_closed


async def test_async_context_manager_closes_http_client():
    async with EntroQJSON("http://localhost") as eq:
        http = eq._http
        assert not http.is_closed

    assert http.is_closed
