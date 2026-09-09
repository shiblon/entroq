"""Doc listing against a real EntroQ server.

Docs takes a nested DocQuery, so its filters are transcoded under the "query."
field path and the server rejects any unprefixed name as an unknown field. That
makes these the only tests that can catch a client/server parameter mismatch:
a unit test asserting the names the client emits merely restates the client's
own belief about them.
"""
import uuid

import pytest

from entroq.json import EntroQJSON
from entroq.types import DocData, Modification


@pytest.fixture
async def seeded(eqmem_url):
    """Yield a client and the docs it inserted, in namespace/key order.

    Each test gets its own namespace, so the session-scoped server needs no
    cleanup between tests.
    """
    async with EntroQJSON(eqmem_url) as eq:
        ns = f"live-{uuid.uuid4().hex}"
        res = await eq.modify(Modification(*[
            Modification.inserting(DocData(
                namespace=ns, key=key, secondary_key=sub, content={"k": key},
            ))
            for key, sub in (("a", "1"), ("b", "1"), ("b", "2"), ("c", "1"))
        ]))
        assert len(res.docs_inserted) == 4
        yield eq, ns


async def test_docs_lists_whole_namespace(seeded):
    eq, ns = seeded

    got = await eq.docs(namespace=ns)

    assert [(d.key, d.secondary_key) for d in got] == [("a", "1"), ("b", "1"), ("b", "2"), ("c", "1")]


async def test_docs_no_filter_is_a_valid_request(seeded):
    """A bare docs() call must reach the server, not fail as an unknown field.

    What an empty namespace *selects* is backend-specific and deliberately not
    asserted here: eqmem reads it as no namespace, while the PostgreSQL backend
    reads it as every namespace.
    """
    eq, _ = seeded

    await eq.docs()  # must not raise


async def test_docs_key_range_is_half_open(seeded):
    eq, ns = seeded

    got = await eq.docs(namespace=ns, key_start="b", key_end="c")

    assert [(d.key, d.secondary_key) for d in got] == [("b", "1"), ("b", "2")]


async def test_docs_key_exact_returns_whole_key_group(seeded):
    """The non-claiming exact read: every doc sharing one primary key."""
    eq, ns = seeded

    got = await eq.docs(namespace=ns, key_exact="b")

    assert [(d.key, d.secondary_key) for d in got] == [("b", "1"), ("b", "2")]
    assert all(d.claimant == "" for d in got), "an exact read must not claim"


async def test_docs_key_exact_missing_key_is_empty(seeded):
    eq, ns = seeded

    assert await eq.docs(namespace=ns, key_exact="nope") == []


async def test_docs_ids_selects_those_docs(seeded):
    eq, ns = seeded
    listed = await eq.docs(namespace=ns)
    want = [listed[0], listed[3]]

    got = await eq.docs(namespace=ns, ids=[d.id for d in want])

    assert [d.id for d in got] == [d.id for d in want]


async def test_docs_limit_caps_results(seeded):
    eq, ns = seeded

    got = await eq.docs(namespace=ns, limit=2)

    assert [(d.key, d.secondary_key) for d in got] == [("a", "1"), ("b", "1")]


async def test_docs_omit_values_drops_content(seeded):
    eq, ns = seeded

    got = await eq.docs(namespace=ns, omit_values=True)

    assert got, "expected docs"
    assert all(d.content is None for d in got)
    assert all(d.key for d in got), "metadata must survive omit_values"
