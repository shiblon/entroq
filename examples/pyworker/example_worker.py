#!/usr/bin/env python3
"""Example showing EntroQ HTTP/JSON client + worker usage.

Requires a running EntroQ service (e.g. eqpg serve or eqmemsvc) at
http://localhost:9100.

Usage:
    python example_worker.py
"""

from entroq.json import EntroQJSON
from entroq.types import TaskData, DependencyError
from entroq.worker import EntroQWorker, StopWorker

QUEUE = '/example/queue'
UPSTREAM = 'http://localhost:9100'


def main():
    eq = EntroQJSON(UPSTREAM)

    inserted, _ = eq.modify(inserts=[
        TaskData(queue=QUEUE, value=b'"task-1"'),
        TaskData(queue=QUEUE, value=b'"task-2"'),
        TaskData(queue=QUEUE, value=b'"task-3"'),
    ])
    print(f'Inserted {len(inserted)} tasks into {QUEUE}')

    # Demonstrate a dependency error: deleting the same task twice.
    t = inserted[0]
    try:
        eq.modify(deletes=[t, t])
        raise AssertionError('expected DependencyError')
    except DependencyError as e:
        print(f'Got expected DependencyError: {e}')

    processed = []

    def handle(renew, finalize):
        with renew as task:
            print(f'Processing task {task.id}: {task.value}')
            processed.append(task.value)
            if len(processed) >= len(inserted):
                raise StopWorker

        with finalize as task:
            eq.modify(deletes=[task])

    w = EntroQWorker(eq)
    w.work(QUEUE, handle)

    print(f'Done. Processed: {processed}')


if __name__ == '__main__':
    main()
