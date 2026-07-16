#!/usr/bin/env python3
"""A resident EntroQ worker over the work gateway, WEBSOCKET transport.

The work itself lives in handler.py, shared with the pipe example
(pipe_worker.py); this file is only the WebSocket transport and its supervision
loop. Unlike the pipe worker -- which spawns its own `eqlink work` child -- a
WebSocket client dials a gateway that is already running as a service:

    eqlink --entroq localhost:37706 work --addr :8080 --queue in --work

The registration (queues, phases) rides in the URL query string, and this worker
survives a restarting backend or a bounced gateway by reconnecting, keyed on the
WebSocket close code the gateway sends: 1013 (try-again) is transient, so
reconnect; every other code (1000 normal, 1008 caller fault, 1011 gateway fault)
stops. See docs/workgateway-protocol.md.

Requires the `websockets` package:  pip install websockets

    GATEWAY_WS_URL=ws://localhost:8080/work?queue=in&work=1 python3 ws_worker.py
"""
import asyncio
import json
import os
import sys
import time

import websockets  # pip install websockets

from handler import handle

WS_URL = os.environ.get("GATEWAY_WS_URL", "ws://localhost:8080/work?queue=in&work=1")

# WebSocket close codes the gateway uses (see ws.go); the one we branch on:
STATUS_TRY_AGAIN = 1013  # transient -- reconnect. Every other code stops.


async def run_once():
    """Dial the gateway, answer phase messages until it closes, return the close code."""
    async with websockets.connect(WS_URL) as ws:
        try:
            async for raw in ws:
                msg = json.loads(raw)
                kind = msg.get("type")
                if kind == "doWork":
                    await ws.send(json.dumps(handle(msg["task"])))
                elif kind == "error":
                    print(f"[worker] gateway error [{msg.get('class')}]: {msg.get('message')}", file=sys.stderr)
                else:
                    print(f"[worker] unexpected message {kind!r}", file=sys.stderr)
        except websockets.ConnectionClosed:
            pass  # a non-normal close raises here; the code is read below either way
        return ws.close_code


async def main():
    backoff = 0.2
    while True:
        started = time.monotonic()
        try:
            code = await run_once()
        except (OSError, websockets.InvalidHandshake) as e:
            # The gateway server itself is unreachable -- transient; retry.
            print(f"[worker] cannot reach gateway ({e}); reconnecting in {backoff:.1f}s", file=sys.stderr)
        else:
            if code != STATUS_TRY_AGAIN:
                print(f"[worker] gateway closed (close {code}); stopping", file=sys.stderr)
                return
            print(f"[worker] gateway unavailable (close {code}); reconnecting in {backoff:.1f}s", file=sys.stderr)
        # A connection that lasted a while before dropping is a fresh outage.
        if time.monotonic() - started > 30:
            backoff = 0.2
        await asyncio.sleep(backoff)
        backoff = min(backoff * 2, 5.0)


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        pass  # the `async with` closes the WebSocket cleanly as it unwinds
