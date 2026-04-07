import logging
import threading
import time
from contextlib import contextmanager
from datetime import datetime, timezone, timedelta
from typing import List, Tuple, Optional, Callable, Iterator, Union

from .types import Task, TaskData, TaskChange, TaskID, DependencyError
from .base import EntroQBase

class StopWorker(Exception):
    """Raise from a work function to stop the worker loop cleanly."""
    pass

class TaskProxy:
    """Proxies attribute access to a task getter, ensuring health checks."""
    def __init__(self, getter):
        self._getter = getter
    def __getattr__(self, name):
        return getattr(self._getter(), name)
    def __repr__(self):
        return repr(self._getter())

@contextmanager
def renewing(client: EntroQBase, task: Task, duration_s: int = 30) -> Iterator[Callable[[], Task]]:
    """Context manager that renews a task claim in the background.

    Yields a callable that returns the current (most recently renewed)
    version of the task.
    """
    exit_event = threading.Event()
    failure_err = [None]
    lock = threading.Lock()
    current = [task]

    def latest() -> Task:
        with lock:
            if failure_err[0] is not None:
                raise failure_err[0]
            return current[0]

    def _renewer():
        exit_event.wait(duration_s / 2)
        while not exit_event.is_set():
            try:
                # To renew, we modify the task with an updated arrival time.
                at = datetime.now(tz=timezone.utc) + timedelta(seconds=duration_s)
                _, changed = client.modify(changes=[latest().as_change(at=at)])
                with lock:
                    current[0] = changed[0]
            except DependencyError as e:
                logging.error("Lease lost for task %s, stopping worker: %s", task.id, e)
                with lock:
                    failure_err[0] = e
                return  # Stop the renewer thread if lease is lost.
            except Exception as e:
                logging.warning("Transient renewal error for task %s (will retry): %s", task.id, e)
            exit_event.wait(duration_s / 2)

    t = threading.Thread(target=_renewer, daemon=True)
    t.start()
    try:
        yield latest
    finally:
        exit_event.set()
        t.join()

class EntroQWorker:
    """Claims tasks from a queue and dispatches them to a handler function.

    Example::

        eq = EntroQJSON('http://localhost:8080')

        def handle(renew, finalize):
            with renew as task:
                result = do_heavy_work(task)

            with finalize as task:
                # task now has a stable version, finish it
                eq.modify(deletes=[task])

        w = EntroQWorker(eq)
        w.work('my-queue', handle)
    """

    def __init__(self, client: EntroQBase):
        self.client = client

    def work(self, queue: Union[str, List[str]], do_func: Callable, claim_duration: int = 30):
        """Loop forever: claim a task and call do_func(renew, finalize) with renewal."""
        while True:
            try:
                task = self.client.claim(queue, duration_ms=1000 * claim_duration)
                final_task = [None]

                @contextmanager
                def renew():
                    with renewing(self.client, task, duration_s=claim_duration) as current_task:
                        yield TaskProxy(current_task)
                    final_task[0] = current_task()

                @contextmanager
                def finalize():
                    if final_task[0] is None:
                        raise RuntimeError("finalize called before renew")
                    yield final_task[0]

                do_func(renew(), finalize())
            except StopWorker:
                return
            except DependencyError as e:
                logging.warning("Worker continuing after dependency error: %s", e)
            except TimeoutError as e:
                logging.warning("Worker continuing after timeout: %s", e)
            except Exception as e:
                logging.exception("Worker loop error: %s", e)
                time.sleep(1)  # Avoid tight error loop
