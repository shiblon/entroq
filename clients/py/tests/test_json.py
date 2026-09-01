from entroq.json import EntroQJSON


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
