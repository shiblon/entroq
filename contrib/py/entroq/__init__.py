# Package entroq provides a client library for working with EntroQ.

import base64
import json
import uuid

from google.protobuf import json_format
import grpc
from grpc_health.v1 import health_pb2
from grpc_health.v1 import health_pb2_grpc

from . import entroq_pb2
from . import entroq_pb2_grpc


class EntroQ:
    def __init__(self, eqaddr):
        """Create an EntroQ client (over gRPC).

        Args:
            eqaddr: Hostport of the address of an EntroQ gRPC service.
        """
        self.addr = eqaddr
        self.claimant_id = str(uuid.uuid4())
        # TODO: allow secure channels.
        self.channel = grpc.insecure_channel(self.addr)
        self.stub = entroq_pb2_grpc.EntroQStub(self.channel)
        self.health_stub = health_pb2_grpc.HealthStub(self.channel)

    @staticmethod
    def to_dict(task, value_type=''):
        if value_type and value_type.lower() == 'json':
            jt = json_format.MessageToDict(task)
            val = jt.get('value')
            if val:
                jt['value'] = json.loads(base64.b64decode(val).decode('utf-8'))
            return jt

        return json_format.MessageToDict(task)

    def queues(self, prefixmatches=(), exactmatches=(), limit=0):
        """Return information about each queue that meets any of the given match criteria.

        If both prefixmatches and exactmatches is empty, then every queue
        matches. If only one is empty, it is simply ignored. The OR of all match
        specs is used to find queue names.

        Args:
            prefixmatches: iterable of allowed prefixes.
            exactmatches: iterable of allowed exact matches.
            limit: return no more than this many matches, all if 0.

        Returns:
            [entroq_pb2.QueueStats]
        """
        resp = self.stub.Queues(entroq_pb2.QueuesRequest(
            match_prefix=prefixmatches,
            match_exact=exactmatches,
            limit=limit))

        return resp.queues

    def tasks(self, queue, claimant_id='', task_ids=(), limit=0):
        """Return tasks that match the given fields. Typically used to itemize a queue.

        Args:
            queue: required queue name.
            claimant_id: optional - if specified, limit to tasks claimed by this claimant.
            task_id: optioanl - if specified, limit to a particular task ID.
            limit: limit to this many results, all if 0.

        Returns:
            [entroq_pb2.Task] for all matching tasks.
        """
        resp = self.stub.Tasks(entroq_pb2.TasksRequest(
            queue=queue,
            claimant_id=claimant_id,
            limit=limit,
            task_id=task_ids))

        return resp.tasks

    def try_claim(self, queue, duration_ms=30000):
        """Try to claim a task from the given queue, for the given duration.

        Args:
            queue: Name of queue to claim a task from.
            duration_ms: Milliseconds that the claim should initially be good for.

        Returns:
            An entroq_pb2.Task if successful, or None if no task could be claimed.
        """
        resp = self.stub.TryClaim(entroq_pb2.ClaimRequest(
            claimant_id=self.claimant_id,
            queue=queue,
            duration_ms=duration_ms))

        return resp.task

    def claim(self, queue, duration_ms=30000, poll_ms=30000):
        """Claim a task, blocking until one is available.

        Args:
            queue: Name of queue to claim a task from.
            duration_ms: Initial duration of task lease, in milliseconds.
            poll_ms: Time between checks if no claim is available, in milliseconds.

        Returns:
            An entroq_pb2.Task when successful.
        """
        # TODO: time out after retry interval, reconnect and try again.
        resp = self.stub.Claim(entroq_pb2.ClaimRequest(
            claimant_id=self.claimant_id,
            queue=queue,
            duration_ms=duration_ms,
            poll_ms=poll_ms))

        return resp.task

    def modify(self, inserts=(), changes=(), deletes=(), depends=()):
        """Attempt a modification of potentially multiple tasks and queues.

        Args:
            inserts: a list of entroq_pb2.TaskData to insert.
            changes: a list of entroq_pb2.TaskChange indicating alterations to tasks.
            deletes: a list of entroq_pb2.TaskID indicating which tasks to delete.
            depends: a list of entroq_pb2.TaskID that must exist for success.

        Returns:
            entroq_pb2.ModifyResponse indicating what was inserted and what was changed.
        """
        # TODO: document exceptions - they are meaningful.
        return self.stub.Modify(entroq_pb2.ModifyRequest(
            claimant_id=self.claimant_id,
            inserts=inserts,
            changes=changes,
            deletes=deletes,
            depends=depends))
