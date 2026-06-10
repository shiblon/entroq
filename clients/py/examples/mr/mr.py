"""MapReduce implementation using EntroQ (async client).

Task values are native JSON values (the client serializes them directly), so a
map input task carries ``{"key": ..., "value": ...}`` and a shard/reduce task
carries a list of those records — no manual encode/decode.

Map and reduce steps are short, so each worker simply claims a task for long
enough to finish it; there is no background renewal to manage.
"""

import asyncio
import hashlib
import logging
import random
import sys
from collections import defaultdict
from itertools import groupby
from operator import itemgetter
from typing import Callable

from entroq import EntroQBase, TaskData, Modification, DependencyError


def fingerprint(key: str) -> int:
    return int(hashlib.md5(key.encode("utf-8")).hexdigest()[:16], 16)


def shard_for_key(key: str, n: int) -> int:
    return fingerprint(key) % max(1, n)


class MapWorker:
    def __init__(self, eq: EntroQBase, input_queue: str, output_prefix: str,
                 mapper_fn: Callable, num_shards: int = 1):
        self.client = eq
        self.input_queue = input_queue
        self.output_prefix = output_prefix
        self.mapper_fn = mapper_fn
        self.num_shards = num_shards

    async def work(self, poll_s: float = 2.0):
        """Process map tasks until the input queue is empty, then retire."""
        logging.info("Starting MapWorker on queue: %s", self.input_queue)
        while True:
            try:
                task = await self.client.try_claim(self.input_queue, duration_ms=30000)
                if task is None:
                    # Only retire once the queue is actually empty.
                    qs = await self.client.queues(exact=[self.input_queue])
                    if (qs[0].get("num_tasks", 0) if qs else 0) == 0:
                        logging.info("MapWorker: queue empty, retiring.")
                        return
                    await asyncio.sleep(poll_s + random.uniform(0, 0.5))
                    continue

                record = task.value  # {"key": ..., "value": ...}
                key = record.get("key", "")
                value = record.get("value", "")

                shards: dict[int, list] = defaultdict(list)

                def emit(k: str, v: str):
                    shards[shard_for_key(k, self.num_shards)].append({"key": k, "value": v})

                self.mapper_fn(key, value, emit)

                ops = [Modification.deleting(task)]
                for shard, items in shards.items():
                    if not items:
                        continue
                    items.sort(key=itemgetter("key"))
                    ops.append(Modification.inserting(
                        TaskData(queue=f"{self.output_prefix}/{shard}", value=items)))

                await self.client.modify(Modification(*ops))

            except DependencyError as e:
                logging.warning("MapWorker dependency error (will retry): %s", e)
            except Exception as e:
                print(f"FATAL: MapWorker crashed: {e}", file=sys.stderr)
                logging.exception("MapWorker task error")
                await asyncio.sleep(1)


class ReduceWorker:
    def __init__(self, eq: EntroQBase, map_empty_queue: str, input_queue: str,
                 output_queue: str, reducer_fn: Callable):
        self.client = eq
        self.map_empty_queue = map_empty_queue
        self.input_queue = input_queue
        self.output_queue = output_queue
        self.reducer_fn = reducer_fn

    async def work(self):
        logging.info("Starting ReduceWorker on queue: %s", self.input_queue)
        # 1. Coalesce all shard tasks for this reducer into one, but only once
        #    the map stage has drained (no more shards will arrive).
        while True:
            try:
                tasks = await self.client.tasks(self.input_queue, limit=200)
                if len(tasks) > 1:
                    merged: list = []
                    for t in tasks:
                        merged.extend(t.value)
                    merged.sort(key=itemgetter("key"))
                    try:
                        await self.client.modify(Modification(
                            Modification.inserting(TaskData(queue=self.input_queue, value=merged)),
                            *(Modification.deleting(t) for t in tasks),
                        ))
                    except DependencyError as e:
                        logging.debug("Merge conflict, retrying: %s", e)
                    continue

                map_qs = await self.client.queues(exact=[self.map_empty_queue])
                if (map_qs[0].get("num_tasks", 0) if map_qs else 0) == 0:
                    if not tasks:
                        logging.info("ReduceWorker: no shards to reduce, exiting.")
                        return
                    break  # map stage drained and a single merged task remains
                await asyncio.sleep(2)

            except Exception as e:
                print(f"FATAL: ReduceWorker merge crash: {e}", file=sys.stderr)
                logging.exception("ReduceWorker merge error")
                await asyncio.sleep(1)

        # 2. Reduce the single coalesced task.
        try:
            task = await self.client.try_claim(self.input_queue, duration_ms=15000)
            if task is None:
                logging.info("ReduceWorker found no task to reduce, exiting.")
                return

            records = task.value  # already sorted by key
            outputs = []
            for k, group in groupby(records, key=itemgetter("key")):
                out_val = self.reducer_fn(k, (g["value"] for g in group))
                if out_val is not None:
                    outputs.append({"key": k, "value": out_val})

            await self.client.modify(Modification(
                Modification.inserting(TaskData(queue=self.output_queue, value=outputs)),
                Modification.deleting(task),
            ))
        except Exception as e:
            print(f"FATAL: ReduceWorker final reduce crash: {e}", file=sys.stderr)
            logging.exception("ReduceWorker reduce error")
