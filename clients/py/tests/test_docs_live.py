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
from entroq.types import DocClaim, DocData, Modification, TaskData


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


async def test_claim_doc_sets_names_sets_and_matches_a_task(seeded):
    """A multi-set claim against a real server, held in step with a task.

    This is the only test that can catch a wrong nested name in the claim
    request: ``claimQuery.sets`` and ``claimQuery.taskToMatch`` are transcoded
    field paths, and a misspelled one claims nothing rather than failing.

    The set with no docs is the point. Its lock comes back regardless, which
    is what makes it renewable; a claim that reported only members would have
    nothing to say about it.
    """
    eq, ns = seeded
    task = await eq.try_claim(
        (await eq.modify(Modification(
            Modification.inserting(TaskData(queue=f"{ns}/q"))))).tasks_inserted[0].queue)
    assert task is not None

    claimed = await eq.claim_doc_sets(
        [DocClaim(ns, "a"), DocClaim(ns, "b"), DocClaim(ns, "empty")],
        task_to_match=task,
    )

    assert [(d.key, d.secondary_key) for d in claimed] == [
        ("a", "1"), ("b", "1"), ("b", "2"),
    ]
    assert [g.key for g in claimed.sets] == ["a", "b", "empty"], (
        "every claimed set comes back, in the order named, members or not")
    assert all(g.is_set_ref() for g in claimed.sets)
    assert all(g.claimant for g in claimed.sets), "all claimed, or none is"
    assert all(g.at == task.at for g in claimed.sets), (
        "a set matched to a task must expire exactly with it")
    # Every set here is at version 1, by the same rule and from different
    # histories: "a" and "b" were created by their inserts at version 0 and
    # moved by this claim, while "empty" was never written, so the claim acts
    # on the empty set that was already there at version 0. A claim is a
    # write; only an insert creates.
    assert [g.version for g in claimed.sets] == [1, 1, 1]


async def test_doc_set_arrival_renews_an_empty_set(seeded):
    """Renewing by set reference holds a set with no docs in it.

    Naming members cannot do this: there are none to name. The arrival travels
    as a duration, so the server resolves it on its own clock.
    """
    eq, ns = seeded
    claimed = await eq.claim_doc_sets([DocClaim(ns, "empty")], duration_ms=60000)
    (empty,) = claimed.sets
    assert list(claimed) == []

    res = await eq.modify(Modification(Modification.arriving(empty, 120.0)))

    (renewed,) = res.docs_changed
    assert renewed.is_set_ref()
    assert (renewed.namespace, renewed.key) == (ns, "empty")
    assert empty.version == 1, (
        "claiming a set nothing has stored moves it off version 0, because it "
        "acts on the empty set that was in effect already there")
    assert renewed.version == 2, "and the renewal moves it again"
    assert renewed.at > empty.at, "and pushes its arrival out"

    released = await eq.modify(Modification(Modification.arriving(renewed, 0.0)))
    (gone,) = released.docs_changed
    assert gone.claimant == "", "a zero arrival releases the set"
