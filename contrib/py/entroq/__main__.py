from .__init__ import EntroQ
from . import entroq_pb2 as pb

import click
import datetime
from datetime import timezone
import grpc
import json

from google.protobuf import json_format

class _ClickContext: pass


@click.group()
@click.option('--svcaddr', default='localhost:37706', show_default=True, help='EntroQ service address')
@click.option('--json', '-j', is_flag=True, default=False, help='Values are JSON, unpack as such for display')
@click.pass_context
def main(ctx, svcaddr, json):
    # TODO: actually use the json flag.
    ctx.ensure_object(_ClickContext)
    ctx.obj.addr = svcaddr


@main.command()
@click.pass_context
@click.option('--queue', '-q', required=True, help='Queue in which to insert a task')
@click.option('--val', '-v', default='', help='Value in task to be inserted')
def ins(ctx, queue, val):
    cli = EntroQ(ctx.obj.addr)
    ins, _ = cli.modify(inserts=[pb.TaskData(queue=queue, value=val.encode('utf-8'))])
    for t in ins:
        print(json_format.MessageToJson(t))


@main.command()
@click.pass_context
@click.option('--prefix', '-p', default='', multiple=True, help='Queue match prefix, if filtering on prefix.')
@click.option('--queue', '-q', default='', multiple=True, help='Exact queue name, if filtering on name.')
@click.option('--limit', '-n', default=0, help='Limit number of results to return.')
def qs(ctx, prefix, queue, limit):
    cli = EntroQ(ctx.obj.addr)
    qs = cli.queues(prefixmatches=prefix, exactmatches=queue, limit=limit)
    qdict = {s.name: json_format.MessageToDict(s) for s in qs}
    print(json.dumps(qdict))


@main.command()
@click.pass_context
@click.option('--task', '-t', required=True, help='Task ID to remove')
@click.option('--force', '-f', is_flag=True, help='UNSAFE: delete task even if claimed already.')
@click.option('--retries', '-r', default=10, help='Number of times to retry if task is claimed.')
def rm(ctx, task, force, retries):
    cli = EntroQ(ctx.obj.addr)
    t = cli.task_by_id(task)
    tid = pb.TaskID(id=t.id, version=t.version)
    cli.delete(task_id=tid,
               unsafe_claimant_id=t.claimant_id if force else None)
    print(json_format.MessageToJson(tid))


@main.command()
@click.pass_context
@click.option('--queue', '-q', required=True, help='Queue to clear')
@click.option('--force', '-f', is_flag=True, help='UNSAFE: delete tasks even if claimed')
def clear(ctx, queue, force):
    cli = EntroQ(ctx.obj.addr)
    for t in cli.pop_all(queue, force=force):
        print(json_format.MessageToJson(t))


@main.command()
@click.pass_context
@click.option('--queue', '-q', multiple=True, help='Queue to claim from (can be multiple)')
@click.option('--try', is_flag=True, help="Only try to claim, don't block")
@click.option('--duration', '-d', default=30, help='Seconds of claim duration')
def claim(ctx, queue, try_, duration):
    cli = EntroQ(ctx.obj.addr)
    claim_func = cli.try_claim if try_ else cli.claim
    t = claim_func(queue, duration=duration)
    print(json_format.MessageToJson(t))


@main.command()
@click.pass_context
@click.option('--millis', '-m', is_flag=True, help="Return time in milliseconds since the Epoch UTC")
@click.option('--local', '-l', is_flag=True, help='Show local time (when not using milliseconds)')
def time(ctx, millis, local):
    cli = EntroQ(ctx.obj.addr)
    time_ms = cli.time()
    if millis:
        print(time_ms)
        return
    tz = None
    if not local:
        tz = timezone.utc
    dt = datetime.datetime.fromtimestamp(time_ms / 1000.0, tz=tz)
    print(dt)


@main.command()
@click.pass_context
@click.option('--queue', '-q', default='', help='Queue to list')
@click.option('--task', '-t', multiple=True, help='Task ID to list')
@click.option('--limit', '-n', default=0, help='Limit returned tasks')
def ts(ctx, queue, task, limit):
    cli = EntroQ(ctx.obj.addr)
    for task in cli.tasks(queue=queue, task_ids=task, limit=limit):
        print(json_format.MessageToJson(task))


@main.command()
@click.pass_context
@click.option('--task', '-t', required=True, help='Task ID to modify - modifies whatever version it finds. Use with care.')
@click.option('--queue_to', '-Q', default='', help='Change queue to this value.')
@click.option('--val', '-v', default='', help='Change to this value.')
@click.option('--force', '-f', is_flag=True, help='UNSAFE: force modification even if this task is claimed.')
def mod(ctx, task, queue_to, val, force):
    cli = EntroQ(ctx.obj.addr)
    t = cli.task_by_id(task)
    old_id = pb.TaskID(id=t.id, version=t.version)
    new_data = pb.TaskData(queue=queue_to or t.queue,
                           value=val or t.value)
    _, chg = cli.modify(changes=[pb.TaskChange(old_id=old_id, new_data=new_data)],
                        unsafe_claimant_id=t.claimant_id if force else None)
    for t in chg:
        print(json_format.MessageToJson(t))


main(obj=_ClickContext())
