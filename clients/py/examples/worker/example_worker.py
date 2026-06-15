#!/usr/bin/env python3
"""Runnable example: insert tasks, then run a worker that processes them.

Run it directly against a live EntroQ JSON endpoint (e.g. ``eqmem serve`` or
``eqpg serve`` listening on http://localhost:9100)::

    python example_worker.py

Or import ``run_demo`` and drive it against any ``EntroQBase`` client. The test
in ``clients/py/tests/test_example_worker.py`` runs this end-to-end against a
real ``eqmem`` subprocess, the way ``go test`` runs the Go worker examples.
"""
from __future__ import annotations

import asyncio

from entroq.base import EntroQBase
from entroq.json import EntroQJSON
from entroq.types import DependencyError, Modification, TaskData, TaskID
from entroq.worker import EntroQWorker

QUEUE = "/example/queue"
DEFAULT_URL = "http://localhost:9100"


async def run_demo(eq: EntroQBase, *, queue: str = QUEUE) -> list:
    """Insert three tasks, show a dependency error, then drain the queue.

    Returns the processed task values in the order they were handled.
    """
    result = await eq.modify(Modification(
        Modification.inserting(TaskData(queue=queue, value="task-1")),
        Modification.inserting(TaskData(queue=queue, value="task-2")),
        Modification.inserting(TaskData(queue=queue, value="task-3")),
    ))
    inserted = result.tasks_inserted
    print(f"Inserted {len(inserted)} tasks into {queue}")

    # Deleting a task at a version that does not exist is a dependency error.
    # Bumping the version pins a task that was never there, so the modify fails
    # and rolls back, leaving every inserted task to be processed below.
    first = inserted[0]
    try:
        await eq.modify(Modification(
            Modification.deleting(TaskID(id=first.id, version=first.version + 1, queue=first.queue)),
        ))
        raise AssertionError("expected DependencyError")
    except DependencyError as e:
        print(f"Got expected DependencyError: {e}")

    # Worker: claim each task, print it, and return a Modification that deletes
    # it. The worker renews the claim in the background while the handler runs
    # and fixes up versions before applying the returned Modification.
    processed: list = []

    @EntroQWorker.handler
    async def process(task, docs):
        processed.append(task.value)
        print(f"Processing {task.id}: {task.value}")
        return Modification(Modification.deleting(task))

    worker = EntroQWorker(eq, queue)
    run_task = asyncio.create_task(worker.run(process))

    # Drive the loop until every task has been handled, then stop it cleanly.
    # stop() unblocks the claim that is otherwise waiting on an empty queue.
    while len(processed) < len(inserted):
        await asyncio.sleep(0.02)
    worker.stop()
    await run_task

    print(f"Done. Processed: {processed}")
    return processed


def main() -> None:
    asyncio.run(run_demo(EntroQJSON(DEFAULT_URL)))


if __name__ == "__main__":
    main()
