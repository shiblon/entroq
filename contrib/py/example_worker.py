#!/usr/bin/env python3

from entroq import EntroQ, EQWorker, DependencyError, as_id
from entroq import entroq_pb2 as pb

def main():
    eq = EntroQ()

    queue = '/test/queue'

    ins, _ = eq.modify(inserts=[
        pb.TaskData(queue=queue,
                    value=b"test1"),
        pb.TaskData(queue=queue,
                    value=b"test2"),
        pb.TaskData(queue=queue,
                    value=b"test3"),
        pb.TaskData(queue=queue,
                    value=b"test4"),
    ])

    # The following tests that we get reasonable errors for two modifications
    # in a row to the same version.
    try:
        t = ins[0]
        print("Doing unspeakable things to {}".format(t.id))
        eq.renew_for(t, 10)
        eq.modify(deletes=[as_id(t)])
        raise ValueError("Expected a dependency error!")
    except DependencyError as e:
        print("Got expected dependency error:\n{}".format(e))

    # Now we show how to use a worker to update something in the outer scope.
    values = []

    def do_stuff(task):
        values.append(task.value)
        print(values)
        return pb.ModifyRequest(deletes=[pb.TaskID(id=task.id)])

    w = EQWorker(eq)
    w.work(queue, do_stuff)

if __name__ == '__main__':
    main()
