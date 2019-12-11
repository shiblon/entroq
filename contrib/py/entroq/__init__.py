"""Package entroq provides a client library for working with EntroQ.
"""

import base64
import json
import threading
import time
import uuid

import grpc

from google.protobuf import json_format
from grpc_health.v1 import health_pb2
from grpc_health.v1 import health_pb2_grpc
from grpc_status import rpc_status

from . import entroq_pb2
from . import entroq_pb2_grpc


def is_cancelled(exc):
    return exc.cancelled()


def is_dependency(exc):
    return exc.code() == grpc.StatusCode.NOT_FOUND


def dependency_error_details(exc, as_json=False):
    if not is_dependency(exc): return None
    # Should have dependency metadata.
    meta = exc.trailing_metadata()
    if not meta:
        return None
    status = rpc_status.from_call(exc)
    details = []
    for d in status.details:
        if not d.type_url.endswith('/proto.ModifyDep'):
            return None
        dep = entroq_pb2.ModifyDep()
        dep.ParseFromString(d.value)
        if dep.type == entroq_pb2.DETAIL and not dep.msg:
            continue
        if as_json:
            details.append(json_format.MessageToDict(dep))
        else:
            details.append(dep)
    return details


def id_str(task_id):
    return '{}:{}'.format(task_id.id, task_id.version)


def as_change(task):
    return entroq_pb2.TaskChange(
        old_id=as_id(task),
        new_data=entroq_pb2.TaskData(
            queue=task.queue,
            at_ms=task.at_ms,
            value=task.value
        )
    )


def as_id(task):
    return entroq_pb2.TaskID(id=task.id, version=task.version)


def as_json_dict(task, unwrap_value=False):
    pass


def as_id(task):
    return entroq_pb2.TaskID(id=task.id, version=task.version)


class DependencyError(Exception):
    @classmethod
    def from_exc(cls, exc):
        deps = dependency_error_details(exc)
        if not deps:
            return exc
        self = cls.from_deps(deps)
        self.exc = exc
        return self

    @classmethod
    def from_deps(cls, deps):
        self = cls()
        for d in deps:
            if d.type == entroq_pb2.CLAIM:
                self.claims.append(id_str(d.id))
            elif d.type == entroq_pb2.DELETE:
                self.deletes.append(id_str(d.id))
            elif d.type == entroq_pb2.CHANGE:
                self.changes.append(id_str(d.id))
            elif d.type == entroq_pb2.DEPEND:
                self.depends.append(id_str(d.id))
            elif d.type == entroq_pb2.DETAIL:
                self.msg = d.msg
            elif d.type == entroq_pb2.INSERT:
                self.inserts.append(id_str(d.id))
        return self

    def __init__(self):
        self.claims = []
        self.deletes = []
        self.changes = []
        self.depends = []
        self.inserts = []
        self.msg = ''

    def as_json(self):
        return json.dumps(self.as_dict())

    def as_dict(self):
        result = {}
        if self.claims:
            result['claims'] = self.claims
        if self.deletes:
            result['deletes'] = self.deletes
        if self.changes:
            result['changes'] = self.changes
        if self.depends:
            result['depends'] = self.depends
        if self.msg:
            result['msg'] = self.msg
        if self.inserts:
            result['inserts'] = self.inserts
        return result

    __str__ = as_json


class EntroQ:
    """Client class for EntroQ over gRPC."""

    def __init__(self, eqaddr='localhost:37706'):
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

        # Call the server, see what time it thinks it is, calculate rough skew.
        now = int(time.time() * 1000)
        self.time_skew = self.time() - now

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
        resp = self.stub.QueueStats(entroq_pb2.QueuesRequest(
            match_prefix=prefixmatches,
            match_exact=exactmatches,
            limit=limit))
        return resp.queues

    def queue_empty(self, queue):
        """Indicate whether the given queue is empty."""
        qs = self.queues(exactmatches=[queue])
        for qstat in qs:
            if qstat.name == queue:
                return qstat.num_tasks == 0
        return True

    def tasks(self, queue='', claimant_id='', task_ids=(), limit=0, omit_values=False):
        """Return tasks that match the given fields. Typically used to itemize a queue.

        Args:
            queue: queue name, if filtering on queue name, otherwise task_ids must be given.
            claimant_id: optional - if specified, limit to tasks claimed by this claimant.
            task_ids: optioanl - if specified, limit to a particular iterable of task IDs.
            limit: limit to this many results, all if 0.
            omit_values: only return metadata.

        Returns:
            [entroq_pb2.Task] for all matching tasks.
        """
        return list(self.stub.StreamTasks(entroq_pb2.TasksRequest(
            queue=queue,
            claimant_id=claimant_id,
            limit=limit,
            task_id=task_ids,
            omit_values=omit_values))
        )

    def task_by_id(self, task_id, queue=''):
        tasks = self.tasks(queue=queue, task_ids=[task_id], limit=1)
        if not tasks:
            raise ValueError("Task {task_id} not found".format(task_id=task_id))
        return tasks[0]

    def try_claim(self, queue, duration_ms=30000):
        """Try to claim a task from the given queue, for the given duration.

        Args:
            queue: Name of queue to claim a task from. Or a list of queue names.
            duration_ms: Milliseconds that the claim should initially be good for.

        Returns:
            An entroq_pb2.Task if successful, or None if no task could be claimed.
        """
        resp = self.stub.TryClaim(entroq_pb2.ClaimRequest(
            claimant_id=self.claimant_id,
            queues=queue if isinstance(queue, (list, tuple)) else [queue],
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
            queues=queue if isinstance(queue, (list, tuple)) else [queue],
            duration_ms=duration_ms,
            poll_ms=poll_ms))

        return resp.task

    def modify(self, inserts=(), changes=(), deletes=(), depends=(), unsafe_claimant_id=None):
        """Attempt a modification of potentially multiple tasks and queues.

        Args:
            inserts: a list of entroq_pb2.TaskData to insert.
            changes: a list of entroq_pb2.TaskChange indicating alterations to tasks.
            deletes: a list of entroq_pb2.TaskID indicating which tasks to delete.
            depends: a list of entroq_pb2.TaskID that must exist for success.
            unsafe_claimant_id: sets the claimant ID to the given value. Use with extreme care. Default is safe.

        Raises:
            grpc.RpcError or, when we can get dependency information, DependencyError.

        Returns:
            (inserted_tasks, changed_tasks) - lists of entroq_pb2.Task.
        """
        try:
            resp = self.stub.Modify(entroq_pb2.ModifyRequest(
                claimant_id=unsafe_claimant_id or self.claimant_id,
                inserts=inserts,
                changes=changes,
                deletes=deletes,
                depends=depends))
            return resp.inserted, resp.changed
        except grpc.RpcError as e:
            raise DependencyError.from_exc(e)

    def delete(self, task_id, unsafe_claimant_id=None):
        """Attempt to delete the given task_id.

        Args:
            task_id: entroq_pb2.TaskID of the task to be deleted.
            unsafe_claimant_id: Specify to override claimant ID (e.g., to force
                deletion of a claimed task). Use with caution.
        """
        self.modify(deletes=[task_id], unsafe_claimant_id=unsafe_claimant_id)

    def time(self):
        res = self.stub.Time(entroq_pb2.TimeRequest())
        return res.time_ms

    def now(self):
        """Return now from the rough perspective of the server, in milliseconds."""
        return int(time.time() * 1000) + self.time_skew

    def renew_for(self, task, duration=30):
        """Renew a task for a given number of seconds."""
        _, chg = self.modify(changes=[
            entroq_pb2.TaskChange(
                old_id=entroq_pb2.TaskID(id=task.id,
                                         version=task.version),
                new_data=entroq_pb2.TaskData(queue=task.queue,
                                             at_ms=self.now() + 1000 * duration,
                                             value=task.value)),
        ])
        return chg[0]

    def do_with_renew(self, task, do_func, duration=30):
        """Calls do_func while renewing the given task.

        Args:
            task: The entroq_pb2.Task to attempt to renew.
            do_func: A function accepting a task and returning anything, to be
                called with this task while it is renewed in the background.
            duration: Claim duration in seconds.


        Returns:
            The (renewed task, do_func result).
        """
        renew_interval = duration // 2
        exit = threading.Event()

        lock = threading.Lock()
        renewed = task

        def renewer():
            exit.wait(duration / 1000)
            while not exit.is_set():
                _t = self.renew_for(task, duration=duration)
                with lock:
                    renewed = _t
                exit.wait(duration / 1000)

        try:
            threading.Thread(target=renewer).start()
            result = do_func(task)
            with lock:
                return renewed, result
        finally:
            exit.set()

    def pop_all(self, queue, force=False):
        """Attempt to completely clear a queue.

        Claims from the queue, deleting everything it claims, until the queue is empty.

        Note that this must be called in a loop.

        Args:
            queue: The queue name to clear.
            force: If specified, will spoof the claimant for every task instead of claiming first.

        Yields:
            Each task that has been removed (entroq_pb2.Task).
        """
        if force:
            for task in self.tasks(queue=queue):
                self.delete(unsafe_claimant_id=task.claimant_id,
                            task_id=entroq_pb2.TaskID(id=task.id, version=task.version))
                yield task
            return

        while not self.queue_empty(queue):
            task = self.claim(queue)
            self.modify(deletes=[entroq_pb2.TaskID(id=task.id, version=task.version)])
            yield task


class EQWorker:
    """Worker for claiming tasks from a given queue and running a given method."""
    def __init__(self, eq):
        """Create a worker using the given EntroQ client.

        Args:
            eq: An EntroQ instance.
        """
        self.eq = eq

    def work(self, queue, do_func, claim_duration=30):
        """Pull tasks from given queue, calling do_func, while renewing claims.

        This function never returns. If you want to run it in the background,
        start up a thrad with this as the target.

        Args:
            queue: The name of the queue to pull from, or a list of queues.
            do_func: The function to call. Accepts a single task argument and
                returns an entroq_pb2.ModifyRequest (no need to specify
                claimant ID).
            claim_duration: Seconds for which this claim should be renewed
                every renewal cycle.
        """
        def fixup(renewed, tlist):
            for val in tlist:
                if val.id == renewed.id and val.version != renewed.version:
                    if val.version > renewed.version:
                        raise ValueError("Task updated inside worker body, version too high")
                    val.version = renewed.version

        while True:
            task = self.eq.claim(queue, duration_ms=1000 * claim_duration)
            try:
                renewed, mod_req = self.eq.do_with_renew(task, do_func, duration=claim_duration)
            except DependencyError as e:
                logging.warn("Worker continuing after dependency: %s", e)
                continue

            if not mod_req:
                logging.info("No modification requested, continuing")
                continue

            if not (mod_req.inserts or mod_req.changes or mod_req.deletes):
                logging.info("No mutating modifications requested, continuing")
                continue

            fixup(renewed, mod_req.changes)
            fixup(renewed, mod_req.depends)
            fixup(renewed, mod_req.deletes)

            self.eq.modify(changes=mod_req.changes,
                           inserts=mod_req.inserts,
                           depends=mod_req.depends,
                           deletes=mod_req.deletes)
