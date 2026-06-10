"""Chaos worker: claims tasks and abandons them so the claim expires.

Demonstrates EntroQ's fault tolerance — a worker that dies mid-task loses
nothing, because the unmodified claim simply expires and the task returns to its
queue for another worker.
"""

import asyncio
import logging
import sys
from typing import List, Union

from entroq import EntroQBase


class ChaosWorker:
    def __init__(self, eq: EntroQBase):
        self.client = eq

    async def work(self, queues: Union[str, List[str]], hold_s: float = 5.0):
        logging.info("Starting ChaosWorker on queues: %s", queues)
        while True:
            try:
                # Short lease: hold the task briefly, then drop it on the floor.
                # With no modify, the claim expires and the task comes back.
                task = await self.client.try_claim(queues, duration_ms=int(hold_s * 2 * 1000))
                if task is None:
                    await asyncio.sleep(1)
                    continue
                logging.warning("CHAOS: claimed %s from %s; plotting its demise...",
                                task.id[:16], task.queue)
                await asyncio.sleep(hold_s)
                logging.warning("CHAOS: dropping %s on the floor.", task.id[:16])
                await asyncio.sleep(1)  # breathe, let real workers in
            except Exception as e:
                print(f"FATAL: ChaosWorker crashed: {e}", file=sys.stderr)
                logging.exception("ChaosWorker error")
                await asyncio.sleep(1)
