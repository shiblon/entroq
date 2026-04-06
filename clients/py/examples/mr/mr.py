"""MapReduce implementation using EntroQ."""

import hashlib
import json
import logging
import random
import sys
import time
from collections import defaultdict
from itertools import groupby
from typing import Callable, Iterator, List, Tuple, Optional
from contextlib import contextmanager
from operator import itemgetter

from entroq import EntroQBase, TaskData, DependencyError
from entroq.worker import renewing

def fingerprint(key: bytes) -> int:
    return int(hashlib.md5(key).hexdigest()[:16], 16)

def shard_for_key(key: bytes, n: int) -> int:
    return fingerprint(key) % max(1, n)

class MapWorker:
    def __init__(self, eq: EntroQBase, input_queue: str, output_prefix: str, mapper_fn: Callable, num_shards: int = 1):
        self.client = eq
        self.input_queue = input_queue
        self.output_prefix = output_prefix
        self.mapper_fn = mapper_fn
        self.num_shards = num_shards

    def work(self, poll_s: float = 2.0):
        """Process map tasks until the input queue is empty."""
        logging.info("Starting MapWorker on queue: %s", self.input_queue)
        while True:
            try:
                task = self.client.try_claim(self.input_queue, duration_ms=10000)
                if task is None:
                    # Be robust: only exit if the queue is actually empty.
                    qs = self.client.queues(exact=[self.input_queue])
                    cnt = qs[0].get("num_tasks", 0) if qs else 0
                    if cnt == 0:
                        logging.info("MapWorker: queue empty (%d tasks), retiring.", cnt)
                        return
                    time.sleep(poll_s + random.uniform(0, 0.5))
                    continue

                with renewing(self.client, task, duration_s=30) as current_task:
                    kv = json.loads(current_task().value)
                    key = kv.get("key", "").encode('utf-8')
                    value = kv.get("value", "").encode('utf-8')

                    shards = defaultdict(list)
                    def emit(k: bytes, v: bytes):
                        shard = shard_for_key(k, self.num_shards)
                        shards[shard].append({"key": k.decode('utf-8'), "value": v.decode('utf-8')})

                    self.mapper_fn(key, value, emit)

                    inserts = []
                    for shard, items in shards.items():
                        if not items:
                            continue
                        items.sort(key=itemgetter("key"))
                        q = f"{self.output_prefix}/{shard}"
                        inserts.append(TaskData(queue=q, value=json.dumps(items).encode('utf-8')))

                    self.client.modify(inserts=inserts, deletes=[current_task()])

            except DependencyError as e:
                logging.warning("MapWorker dependency error (will retry): %s", e)
            except Exception as e:
                print(f"FATAL: MapWorker crashed: {e}", file=sys.stderr)
                logging.exception("MapWorker task error")
                time.sleep(1)

class ReduceWorker:
    def __init__(self, eq: EntroQBase, map_empty_queue: str, input_queue: str, output_queue: str, reducer_fn: Callable):
        self.client = eq
        self.map_empty_queue = map_empty_queue
        self.input_queue = input_queue
        self.output_queue = output_queue
        self.reducer_fn = reducer_fn

    def work(self):
        logging.info("Starting ReduceWorker on queue: %s", self.input_queue)
        while True:
            try:
                # 1. Merge tasks if > 1
                tasks = self.client.tasks(self.input_queue, limit=200)
                if len(tasks) > 1:
                    merged = []
                    for t in tasks:
                        kvs = json.loads(t.value)
                        merged.extend(kvs)
                    
                    merged.sort(key=itemgetter("key"))
                    
                    try:
                        self.client.modify(
                            inserts=[TaskData(queue=self.input_queue, value=json.dumps(merged).encode('utf-8'))],
                            deletes=tasks
                        )
                    except Exception as e:
                        logging.debug("Merge conflict, retrying... %s", e)
                    continue

                if len(tasks) <= 1:
                    # Check map empty queue
                    map_tasks = self.client.queues(exact=[self.map_empty_queue])
                    map_count = map_tasks[0].get("num_tasks", 0) if map_tasks else 0
                    if map_count == 0:
                        if not tasks:
                            logging.info("ReduceWorker: Map stage empty, no shards to reduce. Exiting.")
                            return
                        break # ready to reduce!
                    time.sleep(2)

            except Exception as e:
                print(f"FATAL: ReduceWorker merge crash: {e}", file=sys.stderr)
                logging.exception("ReduceWorker merge error")
                time.sleep(1)

        # 2. Reduce the single task
        try:
            task = self.client.try_claim(self.input_queue, duration_ms=10000)
            if not task:
                logging.info("ReduceWorker found no task to reduce, exiting.")
                return

            with renewing(self.client, task, duration_s=15) as current_task:
                kvs = json.loads(current_task().value)

                outputs = []
                for k, group in groupby(kvs, key=itemgetter("key")):
                    vals = (g["value"].encode('utf-8') for g in group)
                    out_val = self.reducer_fn(k.encode('utf-8'), vals)
                    if out_val is not None:
                        outputs.append({"key": k, "value": out_val.decode('utf-8')})

                out_bytes = json.dumps(outputs).encode('utf-8')

                self.client.modify(
                    inserts=[TaskData(queue=self.output_queue, value=out_bytes)],
                    deletes=[current_task()]
                )
        except Exception as e:
            print(f"FATAL: ReduceWorker final reduce crash: {e}", file=sys.stderr)
            logging.exception("ReduceWorker reduce error")
