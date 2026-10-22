"""MapReduce on EntroQ, built on the worker framework.

Mirrors Go's ``pkg/eqmr``: tasks carry the work to do, docs carry the data
between stages, and each stage is an :class:`~entroq.EntroQWorker` handler. The
worker owns claiming, lease renewal, retries, and quarantine, so a stage is
just a function from a task to the modification that finishes it.

Data layout, all in one namespace per run:

- ``split/NNNNNN``   one doc per input split, holding its text.
- ``mapout/PPPP``    one doc per (partition, split): the records a mapper sent
                     to that partition, named by the split that produced them.
                     A doc map holds many docs under one key, each with its own
                     secondary key, so a partition accumulates one subdoc per
                     contributing split.
- ``result/PPPP``     one doc per partition, holding its reduced output.

The two stages differ in how they exclude a duplicate worker, on purpose:

- A **mapper does not claim** anything. Two mappers may run the same split
  concurrently; the loser's commit is rejected because its version-pinned
  delete of the split doc no longer matches. Mapping again is cheap, so racing
  beats blocking. This is what Go's eqmr does.
- A **reducer claims its partition's doc map** through the handler's selector.
  A reduce reads every record a partition received and may take a while, so
  paying for exclusion up front beats discovering at commit that the work was
  wasted. The worker holds the map in lockstep with the task's lease, renews
  both together, and releases the map if the commit did not write it.
"""

import asyncio
import hashlib
import logging
from collections import defaultdict
from itertools import groupby
from operator import itemgetter
from typing import Callable

from entroq import (
    DocClaim, DocData, EntroQBase, EntroQWorker, Modification, TaskData,
)


def shard_for_key(key: str, n: int) -> int:
    """Return the partition a key belongs to, stable across processes.

    Python's hash() is salted per process, so it cannot be used to partition
    work that several processes have to agree about.
    """
    digest = hashlib.md5(key.encode("utf-8")).hexdigest()[:16]
    return int(digest, 16) % max(1, n)


def split_key(i: int) -> str:
    return f"split/{i:06d}"


def mapout_key(partition: int) -> str:
    return f"mapout/{partition:04d}"


def result_key(partition: int) -> str:
    return f"result/{partition:04d}"


async def seed(eq: EntroQBase, namespace: str, map_queue: str,
               splits: list[str], shards: int) -> int:
    """Insert one split doc and one map task per split. Returns the count."""
    ops: list = []
    for i, text in enumerate(splits):
        key = split_key(i)
        ops.append(Modification.inserting(DocData(
            namespace=namespace, key=key, content=text)))
        ops.append(Modification.inserting(TaskData(
            queue=map_queue,
            value={"ns": namespace, "split": key, "shards": shards})))
    await eq.modify(Modification(*ops))
    return len(splits)


async def seed_reduces(eq: EntroQBase, namespace: str, reduce_queue: str,
                       shards: int) -> None:
    """Insert one reduce task per partition.

    Called once the map stage has drained: a reducer that ran while mappers
    were still emitting would reduce a partial partition and record it as
    final.
    """
    await eq.modify(Modification(*[
        Modification.inserting(TaskData(
            queue=reduce_queue, value={"ns": namespace, "partition": p}))
        for p in range(shards)
    ]))


def map_handler(eq: EntroQBase, mapper_fn: Callable):
    """Build the map stage: run mapper_fn over a split, emit per-partition docs."""

    @EntroQWorker.handler
    async def process(task, docs):
        ref = task.value
        ns, key, shards = ref["ns"], ref["split"], ref["shards"]

        # Read the split without claiming it. A duplicate mapper reads the same
        # doc and races to commit; see the module docstring.
        found = await eq.docs(namespace=ns, key_exact=key)
        if not found:
            # Gone, so another mapper already committed this split. Nothing is
            # left to do but retire the task.
            return Modification(Modification.deleting(task))
        split = found[0]

        buckets: dict[int, list] = defaultdict(list)

        def emit(k: str, v: str) -> None:
            buckets[shard_for_key(k, shards)].append({"key": k, "value": v})

        mapper_fn(key, split.content, emit)

        # Deleting the split at the version just read is what excludes the
        # duplicate: whichever mapper commits first moves the version, and the
        # other's whole modification is rejected.
        ops = [Modification.deleting(task), Modification.deleting(split)]
        for partition, records in buckets.items():
            records.sort(key=itemgetter("key"))
            ops.append(Modification.inserting(DocData(
                namespace=ns,
                key=mapout_key(partition),
                # Named by the split that produced it, so a partition's map
                # holds one subdoc per contributing split.
                secondary_key=key,
                content=records,
            )))
        return Modification(*ops)

    return process


def reduce_handler(reducer_fn: Callable, reduce_delay_s: float = 0.0):
    """Build the reduce stage: claim a partition's map, reduce it, record it.

    Takes no client: everything the reducer reads arrives through the claim,
    which is the point of declaring it in the selector.

    reduce_delay_s makes the reduce outlast its lease, so the worker has to
    renew the task and the claimed map together to keep them.
    """

    @EntroQWorker.handler
    async def process(task, docs):
        ref = task.value
        ns, partition = ref["ns"], ref["partition"]

        if reduce_delay_s:
            await asyncio.sleep(reduce_delay_s)

        # docs holds the partition's subdocs, one per split that emitted to it,
        # already claimed on this task's behalf. A partition nobody emitted to
        # is an empty map: claimed just the same, with no members.
        records: list = []
        for doc in docs:
            records.extend(doc.content)
        records.sort(key=itemgetter("key"))

        outputs = []
        for k, group in groupby(records, key=itemgetter("key")):
            value = reducer_fn(k, (g["value"] for g in group))
            if value is not None:
                outputs.append({"key": k, "value": value})

        # The result is written even when the partition was empty, so "this
        # partition is finished" is a recorded fact rather than an absence.
        ops = [Modification.deleting(task)]
        ops.extend(Modification.deleting(d) for d in docs)
        ops.append(Modification.inserting(DocData(
            namespace=ns, key=result_key(partition), content=outputs)))
        return Modification(*ops)

    @process.selector
    async def process(task):
        # One claim, naming the partition's map. The worker holds it until this
        # task arrives, so the map cannot outlive the claim on the task.
        return [DocClaim(task.value["ns"], mapout_key(task.value["partition"]))]

    return process


async def run_mapper(eq: EntroQBase, map_queue: str, mapper_fn: Callable,
                     claim_duration_s: float = 2.0) -> EntroQWorker:
    """Run a map worker until stopped. Returns the worker so callers can stop it."""
    worker = EntroQWorker(eq, map_queue, claim_duration_s=claim_duration_s)
    logging.info("map worker starting on %s", map_queue)
    await worker.run(map_handler(eq, mapper_fn))
    return worker


async def run_reducer(eq: EntroQBase, reduce_queue: str, reducer_fn: Callable,
                      claim_duration_s: float = 2.0,
                      reduce_delay_s: float = 0.0) -> EntroQWorker:
    """Run a reduce worker until stopped."""
    worker = EntroQWorker(eq, reduce_queue, claim_duration_s=claim_duration_s)
    logging.info("reduce worker starting on %s", reduce_queue)
    await worker.run(reduce_handler(reducer_fn, reduce_delay_s))
    return worker


async def results(eq: EntroQBase, namespace: str, shards: int) -> dict:
    """Return the merged output of every partition that has finished."""
    out: dict = {}
    for p in range(shards):
        for doc in await eq.docs(namespace=namespace, key_exact=result_key(p)):
            for record in doc.content:
                out[record["key"]] = record["value"]
    return out
