from .__init__ import EntroQ
from . import entroq_pb2

import click
import grpc
import json

from google.protobuf import json_format

class _ClickContext: pass


@click.group()
@click.option('--svcaddr', default='localhost:37706', show_default=True, help='EntroQ service address')
@click.option('--json', '-j', is_flag=True, default=False, help='Values are JSON, unpack as such for display')
@click.pass_context
def main(ctx, svcaddr, json):
    ctx.ensure_object(_ClickContext)
    ctx.obj.addr = svcaddr


@main.command()
@click.pass_context
@click.option('--queue', '-q', required=True, help='Queue in which to insert a task')
@click.option('--val', '-v', default='', help='Value in task to be inserted')
def ins(ctx, queue, val):
    cli = EntroQ(ctx.obj.addr)
    ins, _ = cli.modify(inserts=[entroq_pb2.TaskData(queue=queue, value=val.encode('utf-8'))])
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


main(obj=_ClickContext())
