"""Chaos worker that claims tasks and drops them without finalizing."""

import logging
import random
import sys
import time
from typing import Union, List
from entroq import EntroQBase
from entroq.worker import EntroQWorker

class ChaosWorker:
    def __init__(self, eq: EntroQBase):
        self.worker = EntroQWorker(eq)

    def work(self, queue: Union[str, List[str]], sleep_time_s: int = 5):
        logging.info("Starting ChaosWorker on queues: %s", queue)
        def handle(renew, finalize):
            try:
                with renew as task:
                    if task is None:
                        return
                    logging.warning("CHAOS: Claimed task %s from %s. Plotting its demise...", task.id[:16], task.queue)
                    time.sleep(sleep_time_s)
                
                # Intentionally NOT using finalize. The renew loop stops, 
                # and the claim will naturally expire.
                if task is not None:
                    logging.warning("CHAOS: Dropping task %s on the floor.", task.id[:16])
                
                time.sleep(1) # Breathe to let other workers in!
            except Exception as e:
                print(f"FATAL: ChaosWorker crashed: {e}", file=sys.stderr)
                logging.exception("ChaosWorker error")

        self.worker.work(queue, handle)
