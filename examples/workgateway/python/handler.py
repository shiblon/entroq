"""The worker's business logic, kept apart from the transport.

`handle` receives the protojson of an api.Task and returns the reply that
settles it. Replace its body with real work.
"""
import sys


def handle(task):
    """Process one task and return the reply that settles it."""
    # `task` is the protojson of an api.Task; its JSON payload is under "value".
    print(f"[worker] task {task.get('id')}: value={task.get('value')!r}", file=sys.stderr)

    # The modification is a protojson ModifyRequest, and it says everything
    # about the task's fate: a task this worker consumed is one it deletes. To
    # produce new work, add "inserts"; leave "claimant_id" empty, because the
    # gateway owns the claim. To give up on the task instead, reply with an
    # "outcome" of "retry" or "move" and an "error" saying why.
    return {
        "type": "modify",
        "modification": {
            "deletes": [{
                "queue": task["queue"],
                "id": task["id"],
                "version": task.get("version", 0),
            }],
        },
    }
