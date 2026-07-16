"""The worker's business logic, shared by both transport examples.

A gateway client's actual work lives in one place; the transport (a stdio pipe or
a WebSocket) is a separate concern. `handle` receives the protojson of an
api.Task and returns the result message to send back. Replace its body with real
work.
"""
import sys


def handle(task):
    """Process one task and return its result message."""
    # `task` is the protojson of an api.Task; its JSON payload is under "value".
    print(f"[worker] task {task.get('id')}: value={task.get('value')!r}", file=sys.stderr)

    # "ok" commits; "ack" is the shorthand for "I consumed this task", so the
    # gateway deletes it for us. To produce new work instead, add a "modification"
    # (a protojson ModifyRequest) and leave its claimant_id empty.
    return {"type": "result", "outcome": "ok", "ack": True}
