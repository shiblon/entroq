# Work gateway protocol 2 — sketch

**Status: proposed, not built.** Protocol 1 is what
[workgateway-protocol.md](workgateway-protocol.md) describes and what
`pkg/workgateway` implements today. This replaces it rather than extending it.

## The one idea

**The client drives every exchange.** Each client message is *"here is my
answer, what next?"*; each server message is an instruction. Neither side ever
sends anything unprompted.

The handshake is not a separate phase. It is the first two turns of the
alternation that then runs for the life of the session:

```
client  hello     { protocols, … }
server            capabilities { protocol, capabilities, session, claimant }
client  config    { queues, phases, … }
server            doWork   { task }           ← the first task
client  result    { outcome, modification }
server            doWork   { task }           ← the next one
client  result    { … }
server            takeDocs { task }
client  docs      { claims }
server            doWork   { task, sets }
client  result    { … }
server            drain                       ← or: nothing more is coming
```

There is no `ready` message: the server's answer to `config` **is** the first
instruction.

### What that buys

- **The server needs no connection-event handler.** It listens for requests.
  There is no "who speaks first" rule to get wrong, and no code path that exists
  only because a connection just opened.
- **Turns alternate the whole way through.** Protocol 1 has an asymmetry — the
  gateway announces itself, then later pushes instructions — which is where its
  request/reply bookkeeping comes from.
- **A worker is a loop, a switch, and a POST.** That is the design constraint:
  the gateway is the path every language other than Go and Python gets worker
  semantics through, so the protocol has to be easy to get working and hard to
  get badly wrong.

```python
session = None
body = {"type": "hello", "protocols": [2]}
while True:
    reply = post("/work", body, session)        # session id in a header
    match reply["type"]:
        case "capabilities": session = reply["session"]; body = my_config
        case "takeDocs":     body = {"type": "docs", "claims": docs_for(reply["task"])}
        case "doWork":       body = {"type": "result", **do(reply["task"], reply["sets"])}
        case "drain":        break
        case "error":        raise GatewayRefused(reply["message"])
```

## The server never pushes

Protocol 1 had exactly one unprompted server message, `abort`, and it is
**deleted**. Its job was to tell a worker to stop working on a task whose claim
had been lost; the replacement is to **hang up**. The only genuinely
out-of-band need is forcing the client to start over, and a dropped connection
is that — on a pipe too, where the parent restarts the child.

Nothing of value is lost. A result for a task whose claim is gone cannot be
committed, which is why protocol 1 already says the gateway discards it. And
`abort` was best-effort by its own admission: a worker that handles one task at
a time "sees the `abort` only after it has answered."

Deleting it also removes the request/reply race at `workgateway.go:218` rather
than fixing it. With no abort there is no "use the reply or abort it?" fork, so
*abandoned* and *gone* become one state, and `awaitAbort`, `giveUp`, the
`ending` latch and the stray-reply bookkeeping go with it.

**Drain, the next thing that would want to push, does not need to.** The server
answers the next request with `drain` instead of a task. That is better than a
push: the client is listening at exactly that moment, by construction.

## Transport

**Plain HTTP request/response.** Not long-polling as a workaround — this is the
protocol's actual shape. A `POST` whose answer may take a while is a `POST`.

- No WebSocket, no duplex, no framing, no upgrade.
- One piece of client state: the session id from `capabilities`, sent as a
  header on every subsequent request.
- Any language with an HTTP client can implement it.

Two alternatives were considered and rejected:

| | why not |
|---|---|
| raw HTTP/2 bidi streams | Full duplex is the *thin* part of HTTP/2 support, not the well-supported part: `fetch` requires `duplex: "half"`, Python's `requests` cannot stream a body while reading a response, and browsers effectively cannot do it at all. |
| gRPC | This is the version of HTTP/2 *with* a good client story, and EntroQ already ships the protos. But it contradicts the gateway's reason to exist: a worker "never touches EntroQ, gRPC, or the queue API." |

Duplex was only ever needed for `abort`. With `abort` gone, nothing requires it.

### Pipes

The same protocol, unchanged. "Connect" means "launched". The child process —
which is the **server**, `eqlink work` — reads stdin first and writes nothing
until it is asked something. That removes protocol 1's awkwardness of a child
announcing itself into a stdout nobody was necessarily reading yet.

The roles invert easily in conversation, so to be explicit: the **parent** that
spawns the child is the worker (the client); the **child** is the gateway (the
server).

## Configuration

Base configuration is **out of band**, which keeps a bad registration cheap to
refuse:

- **stdio** — environment and flags when exec'ing the child.
- **HTTP** — URL parameters when connecting.

The `config` message may then **shadow or append** to that base, now that the
client has seen what the server offers in `capabilities`. A stricter rule (only
adjust what the capabilities response revealed) was considered and deliberately
deferred until there is evidence about what callers actually adjust.

Two constraints:

- **The lease is gateway-owned and not client-alterable.** `eqlink work --lease`
  already documents this, and a client that could move it could pin a task for
  as long as it liked.
- **The in-band validation is the authoritative one.** Rejecting a bad
  registration before the connection is established is an early-rejection
  convenience, not a safety property, because `config` can change things
  afterwards.

## Negotiation and refusal

`hello` names the protocols **the client** speaks, so the server can refuse an
incompatible one immediately. The server can likewise refuse a `config` it does
not accept. Either refusal is an `error` instruction naming the fault, followed
by the connection closing.

If neither is refused, the session is good and a task is coming.

The gateway's protocol number is its own, distinct from `version.Protocol`,
which clients speak to the EntroQ service. Conflating the two caused a real bug
(`23d6697`), so they stay separate and are negotiated separately.

## What the capabilities response carries

- The protocol chosen from the client's list.
- The capabilities the server offers, which the `config` may then opt into.
- The session id.
- The **claimant** this connection holds its tasks and doc sets as
  (`Bridge.Claimant`). One connection is one consumer, named after itself, so
  several connections on one EntroQ client do not claim as each other. It is
  reported because it is what identifies the holder in stored tasks and in the
  gateway's metrics.

## Open

- Whether `error` is an instruction type or an HTTP status (or both — a status
  for transport-level refusal, an `error` instruction for protocol-level).
- Session expiry: how long a server holds session state for a client that stops
  asking, versus letting the task's lease decide.
- Whether `takeDocs` and `doWork` can be collapsed, given the client now asks
  for work and could declare its doc needs in the same breath.
