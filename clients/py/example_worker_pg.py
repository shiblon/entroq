#!/usr/bin/env python3
"""Example showing direct-PostgreSQL EntroQ usage."""

import json
from entroq_pg import EntroQ, EQWorker, TaskData, DependencyError

CONNSTR = 'host=localhost dbname=entroq user=entroq password=entroq'
QUEUE = '/test/queue'


def main():
    eq = EntroQ(CONNSTR)

    # Insert a handful of tasks.
    inserted, _ = eq.modify(inserts=[
        TaskData(queue=QUEUE, value=b'task1'),
        TaskData(queue=QUEUE, value=b'task2'),
        TaskData(queue=QUEUE, value=b'task3'),
    ])
    print(f'Inserted {len(inserted)} tasks')

    # Demonstrate a dependency error: delete the same task twice.
    t = inserted[0]
    try:
        eq.modify(deletes=[t.as_id()])   # fine
        eq.modify(deletes=[t.as_id()])   # version is now stale - error
        raise AssertionError('Expected DependencyError')
    except DependencyError as e:
        print(f'Got expected DependencyError: {e}')

    # Worker: claim tasks and move them to a done queue, atomically updating
    # a hypothetical application table in the same transaction.
    def handle(task):
        # task is a callable - always returns the latest renewed version.
        current = task()
        print(f'Processing task {current.id}: {current.value}')
        result = json.dumps({'processed': current.value.decode()}).encode()
        with eq.transaction() as txn:
            # Any user SQL can go here, in the same transaction as the modify.
            # txn.conn.execute("UPDATE my_table SET status='done' WHERE ...")
            txn.modify(
                deletes=[task().as_id()],
                inserts=[TaskData(queue='/done', value=result)],
            )

    w = EQWorker(eq)
    w.work(QUEUE, handle)


if __name__ == '__main__':
    main()
