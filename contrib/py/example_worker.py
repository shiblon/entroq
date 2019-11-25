#!/usr/bin/env python3

from entroq import EntroQ, EQWorker
from entroq import entroq_pb2 as pb

def main():
    eq = EntroQ()

    queue = '/test/queue'

    eq.modify(inserts=[
        pb.TaskData(queue=queue,
                    value=b"test1"),
        pb.TaskData(queue=queue,
                    value=b"test2"),
        pb.TaskData(queue=queue,
                    value=b"test3"),
        pb.TaskData(queue=queue,
                    value=b"test4"),
    ])

    values = []

    def do_stuff(task):
        values.append(task.value)
        print(values)
        return pb.ModifyRequest(deletes=[pb.TaskID(id=task.id)])

    w = EQWorker(eq)
    w.work(queue, do_stuff)

if __name__ == '__main__':
    main()
