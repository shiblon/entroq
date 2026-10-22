# Work Gateway Protocol

> [!WARNING]
> The work gateway is experimental. Its wire protocol and its behavior under
> cancellation and disconnection may change without backward compatibility.

The work gateway (`eqlink work`, package `workgateway`) lets a worker written in
any language run the EntroQ worker loop without importing EntroQ, gRPC, or the
queue API. The gateway runs the hard, stateful part in Go: claim, renew,
stop-and-freeze before commit, version fix-up, retry, move and backoff, doc set
claims. A foreign worker connects and answers a small JSON protocol.

There are two kinds of user, and the protocol is shaped for both:

- A **library author** wraps this protocol in a client library for their
  language, hiding the transport and reconnection behind a "register handlers,
  run" API. This document is written for them.
- A **library user** imports that library, registers handler functions, and
  runs. They should never see any of what follows.

The wire types and their field comments live in `pkg/workgateway`
(`workgateway.go`, `outcome.go`, `wire.go`). A complete client in about 500
lines of standard-library Python is `examples/workgateway/python/eqgateway.py`.
This note is the contract and the reasons for it.

---

## The client drives

Every client message is an answer and a request: "here is my answer, what
next?" Every gateway message answers the request before it. The gateway never
speaks unprompted, and every request gets exactly one answer.

That makes a worker a loop and a switch around one blocking call:

```python
reply = exchange({"type": "config", "config": my_config})
while True:
    match reply["type"]:
        case "ready":      msg = {"type": "ready"}
        case "take":       msg = {"type": "take", "doc_set_claims": claims_for(reply["task"])}
        case "work":       msg = {"type": "modify", **do(reply["task"], reply.get("doc_sets", []))}
        case "success" | "dependency":
                           msg = {"type": "done"}
        case "quit":       return
        case "error":      raise GatewayError(reply["class"], reply["message"])
    reply = exchange(msg)
```

Nothing in the protocol needs a connection event, a duplex stream, or a reader
that runs while the worker works. It runs over a pipe and over plain HTTP
unchanged (see [Transports](#transports)).

## The payloads are the EntroQ protobufs, as protojson

Every domain object on the wire (task, doc, doc set, modification,
dependency, and the read requests and answers) is the canonical protojson of
the message of that name in `api/entroq.proto`. Only the envelope around them,
a `type` and the fields this document names, belongs to the gateway.

A worker needs nothing from the proto to read or write them: they are plain
JSON, and naming only the fields a worker uses is enough. Generating types
from the proto works too, but is never required; the examples write theirs by
hand. Field names are lowerCamelCase (`claimantId`, `byMs`), though the
gateway also accepts the proto's own snake_case. int64 fields arrive as
strings, and the gateway accepts strings or numbers.

Three rules for a modification, all enforced:

- **Leave `claimant_id` empty.** The gateway owns the claim and attributes the
  commit itself; a modification that names a claimant is refused.
- **Name the queue on every task reference.** An insert, change, delete, or
  depend without its queue is refused, because authorization is decided per
  queue and the gateway will not guess one.
- **Arrivals are durations (`by_ms`), never instants.** The server resolves
  them on its own clock.

`ok` alone does not delete the task in hand. Deleting it, usually alongside
whatever the work produced, is part of the modification.

## A session

A session is one worker slot: at most one task in hand, strict alternation, no
correlation IDs. Concurrency is more sessions, each with its own claimant.

### It opens with a config

The first message is `config`, carrying the worker's registration, fixed for
the session:

| Field | Meaning |
|---|---|
| `queues` | Queues to claim from. Required. |
| `protocol_versions` | The gateway protocols this client speaks. Required; this gateway speaks `1`. |
| `want_docs` | Run the take turn, so the client names doc sets to claim. |
| `want_success`, `want_dependency` | Run the post-commit turns. |
| `error_queue` | Where a quarantined or moved task goes. `{inbox}` stands for the task's own queue; the default is `{inbox}/err`. |
| `max_attempts` | Retries before a task is quarantined. Zero is no limit. |
| `max_claims` | Claims a task may fail to survive before it is quarantined. Zero takes the worker's default of five; a negative value is no limit. |
| `retry_delay_s` | Base delay, in whole seconds, before a retried task is available again, including a task whose doc sets someone else holds. Zero takes the worker's default of 30. |
| `work_timeout_s` | How long this client expects one task to take, in whole seconds. The operator caps it. Zero takes the operator's cap, `--max-work-timeout` (default 5m). |
| `dangerous_claimant_override` | Replace the gateway's claimant. Only if you supply your own ownership guarantees. |

The gateway answers with `ready`, which carries what the session settled on:

| Field | Meaning |
|---|---|
| `session` | The session id. Over HTTP, every later message carries it. |
| `claimant` | The consumer this session holds tasks and doc sets as. |
| `work_timeout_s` | The work timeout actually granted: the client's, capped by the operator's `--max-work-timeout`. |
| `silence_timeout_s` | The silence window, two leases long. |

A config the gateway will not serve is answered with an `error` of class
`caller`, and the session ends. The gateway protocol number is its own,
separate from the EntroQ service protocol, and the two are negotiated
separately.

The client answers `ready` with a `ready` of its own, and the gateway starts
claiming.

### The turns

| Gateway sends | Client answers |
|---|---|
| `ready` | `ready` |
| `take {task}` | `take {doc_set_claims}`, or an outcome that settles the task |
| `work {task, doc_sets}` | `modify {modification}`, or an outcome |
| `success` | `done`, with an optional outcome |
| `dependency {deps}` | `done`, with an optional outcome |
| `quit` | nothing: the session is over |
| `error` | nothing: the session is over |

```
client  config {queues, protocol_versions, ...}
gateway ready  {session, claimant, work_timeout_s, silence_timeout_s}
client  ready
  (the gateway claims a task and begins renewing it)
gateway take   {task}                       # only with want_docs
client  take   {doc_set_claims}
gateway work   {task, doc_sets}
client  modify {modification}
  (the gateway stops renewal and commits)
gateway success                             # only with want_success
client  done
  (the gateway claims the next task)
gateway work   {task, doc_sets}
...
```

`take` and `work` come before the commit and are named for what the client
does. `success` and `dependency` come after it and are named for what
happened; exactly one of them fires, and only if the client asked for it.

The commit is the exactly-once boundary. Everything before it is at least
once: a task whose answer never lands is claimed again. `success` after it is
at most once, so whatever the client does there must be safe to skip or to
repeat.

Each gateway message also carries `version`, the protocol the session speaks,
and `expect`, the type it wants back, which a client may use to catch its own
mistakes.

### Outcomes

`take`, `modify` and `done` may carry an `outcome`, the vocabulary a native Go
worker has, with an `error` saying why:

| Outcome | Effect |
|---|---|
| (none) | Commit the modification, which may be empty. |
| `retry` | Re-queue the task with backoff and count the attempt; quarantine it when the attempts run out. `error.queue` names where an exhausted task goes. |
| `move` | Quarantine the task now, to `error.queue` or the session's error queue. |
| `fatal` | Stop the session. The task keeps its claim until the lease lapses. |
| `error` | A failure the client cannot classify, such as an exception it did not expect. The session stops, quarantining the task first if this claim was its last. |

```json
{"type": "modify", "outcome": "retry", "error": {"message": "upstream 503"}}
```

Retry and quarantine are one decision, taken by the gateway at the moment of
failure. A client should never re-queue a task itself.

On `done`, an outcome acts on the task after the commit: optimistically after
a lost dependency race, where it lands only if the task was not itself what
went missing.

### Doc sets

With `want_docs`, each task starts with a `take` turn. The client names the
doc sets the task needs, each as a `DocSetClaim`:

```json
{"type": "take", "doc_set_claims": [{"set": {"namespace": "ns", "key": "k"}}]}
```

The gateway claims them all or none, as the task's claimant, until the task's
own arrival. A set held by someone else is retried with backoff. `work` then
carries each claimed set as `{"set": <the set's lock, as a Doc>, "docs":
[...]}`. `omit_members` claims a set without returning its docs.

### Reads within a turn

While it decides, a client may ask questions: `time`, `tasks`, `docs`,
`queues`, `namespaces`. Each carries the gRPC service's request message
(`tasks_query`, `docs_query`, `match_query`) and is answered with the service's
response content (`time_ms`, `tasks`, `docs`, `queues`, `namespaces`). A read
is not a turn: the turn ends only when the answer it is waiting for arrives,
and a client may read as often as it likes before that.

A read that fails is answered with an `error` carrying a class, and the
session goes on with the task untouched.

### Long work: the work timeout and the silence window

A client that takes its task and dies reports nothing, so the work timeout
bounds every task: the client asks for one in its config, the operator caps
it, and `ready` reports what was granted. When it fires, the task goes back
with its attempt counted, and the session ends rather than go round again
without a client that may still be working.

A client whose work is long but whose liveness should be checked sooner sends
`working`, answered with `ok` at no cost to the backend. The first `working`
during a task arms the silence window (`silence_timeout_s`, two leases): from
then on, any request must come within the window of the last, or the task goes
back with its attempt counted and the session ends. Send one every third of
the window, so one late beat is not fatal. The next task starts unarmed.

### Waiting for a task: the idle ready

Every request is owed an answer, including the `ready` that starts the
claiming, and a queue may stay empty for a long time. So once a request has
waited a third of the silence window while the gateway claims, the gateway
answers it with `ready`, and the client answers that with `ready` again. Over
HTTP this keeps a waiting POST from outliving whatever stands between the
client and the gateway; over a pipe it tells the client the gateway is still
there.

The client must answer within the silence window. One that does not has gone:
the gateway stops claiming and the session ends, with nothing more sent. A task
that is claimed while the client holds the turn waits for the client's `ready`,
then answers it. The answer to an idle `ready` must be `ready`; anything else
is a `caller` fault.

## Stopping

**Quit.** Any message may carry `"quit": true`, and it holds from then on. The
gateway drains: it claims nothing more, the task in hand finishes through every
turn it has left, and the last request is answered with `quit`. On a `config` or
a `ready` there is nothing in hand, and the answer is `quit` at once. A client
sets it the moment it decides to stop.

**Hanging up** is a clean stop too. A client that hangs up while the gateway
claims stops the claiming. A task it holds unanswered keeps its claim until the
lease lapses. An answer the client sent before it hung up still commits.

**The operator** stops `eqlink work` with a signal. The first `SIGINT` or
`SIGTERM` drains every session as a quit would, answering each with `quit`,
and answers any config that arrives meanwhile with `quit`; it waits up to
`--lease`. A second signal stops at once, leaving any task in hand to its
lease. A library that spawns the gateway forwards its own signals to it and
answers until each session says `quit`. It runs the gateway in a process
session of its own, so a terminal's Ctrl-C reaches the gateway once, through
the library; under a supervisor, signal the worker process alone (systemd
`KillMode=mixed`).

## Transports

The messages are the same on both. `eqlink work` takes the operator's settings
as flags: `--lease`, `--max-work-timeout`, `--max-frame-mb`, and the EntroQ
connection flags shared by every `eqlink` command.

### Stdio

Without `--addr`, `eqlink work` runs one session over its stdin and stdout, one
JSON value per line. A client spawns it and writes to its stdin. Diagnostics go
to stderr, which a spawning client should inherit. The session ends when the
client closes stdin, and the process exits with the session's class (below).

### HTTP

With `--addr`, `eqlink work` serves sessions over HTTP. Once listening, it
writes one line to stdout and nothing after it, so a parent can pass port 0:

```json
{"addr":"127.0.0.1:43117"}
```

Each message is a POST of its JSON body; the response body is the gateway's
answer. Every message after the config carries `"session"` from the `ready`. A
session spans as many connections as the client likes: between tasks nothing is
in flight, and a client working on a task may come and go. A POST waiting on a
claim is a request in flight; a client that stops waiting for it has hung up.

The gateway stops when its parent exits (`--parent-pid`, or the parent it
started with) unless `--detached`, for a gateway run as a service.

### Frames

One message, in either direction, is at most `--max-frame-mb` (10 MB by
default, matching `eqserve`). Over a pipe a longer line, or one that does not
decode as a request, ends the session with a `caller` error, since a line
abandoned part-read leaves no point to resynchronize from. Over HTTP it is
refused with 413, or 400 for a body that does not decode.

## Errors

A native Go worker inspects an error's type and branches. A foreign worker
cannot, so the gateway reports the same information as a class:

| Class | Meaning | What a client does |
|---|---|---|
| `transient` | EntroQ unreachable or restarting. | Back off and start a new session. |
| `caller` | The client's fault: a refused config, a protocol violation, a `fatal` outcome. | Stop and surface it; a person fixes it. |
| `gateway` | An unexpected error inside the gateway. | Stop and surface it; likely a bug to report. |
| `gone` | HTTP only: the request named a session the gateway does not hold. | Start a new session at once. |

A session that ends on an error answers the request it ended on with
`{"type": "error", "class": ..., "message": ...}`. The class also travels as:

- **An exit code**, from `eqlink work` over a pipe, from `sysexits.h`: `0` clean,
  `75` transient, `78` caller, `70` gateway.
- **An HTTP status.** Every answer is a JSON message, an error as much as any
  other: `400` caller, `503` transient, `500` gateway, `404` gone, and `405` or
  `413` (class `caller`) for a request that is not a POST or is too large. A
  client that stopped waiting gets no answer at all.
- **A start-up line.** `eqlink work` that fails before a client owns its stdout
  (a bad flag, EntroQ unreachable, a failed listen) writes the error there as
  one line, where the address or the first answer would have been, then exits
  with the class code.

Branch on the class, never on the code or the message. A body that does not
decode as the gateway's JSON (a proxy's error page, say) is classed by its
status.

## The failure contract

What the gateway does when something goes wrong, and what becomes of the task.
"Its lease" means held until the lease lapses, then claimable again. The test
named in each row pins it, in `pkg/workgateway` unless noted.

| Situation | Gateway | Task | Class |
|---|---|---|---|
| Client hangs up while the gateway claims | stops claiming (`TestStreamIdleHangUpStopsClaiming`, `TestHTTPIdleHangUpStopsClaiming`) | none claimed | clean |
| Client misses an idle `ready` | stops claiming, ends the session, sends nothing (`TestIdleSilenceEndsTheSession`) | none claimed | clean |
| Pipe client hangs up holding `take` or `work` | ends the session (`TestStreamHangUpFreesTheTask`) | its lease | clean |
| HTTP client away between POSTs while it works | nothing: renewal goes on (`TestHTTPWorkSurvivesDisconnection`) | held | none |
| Client sends its answer, then hangs up | commits it (`TestStreamHangUpAfterAnswerStillCommits`, `TestHTTPHangUpAfterAnswerStillCommits`) | committed | clean |
| No answer within the work timeout | ends the session; over HTTP the next request is `gone` (`TestStreamWorkTimeoutEndsTheSession`, `TestHTTPWorkTimeoutEndsTheSession`) | retried, attempt counted | clean |
| Armed client silent for the silence window | ends the session (`TestSilenceAfterWorkingEndsTheSession`) | retried, attempt counted | clean |
| Client answers with the wrong type | ends the session, answering why (`TestWrongAnswerIsTheCallersFault`) | its lease | `caller` |
| Client answers an idle `ready` with anything else | ends the session (`TestIdleTurnTakesOnlyAReady`) | none claimed | `caller` |
| Client reports `fatal` | ends the session, answering the request (`TestFatalOutcomeIsAnswered`) | its lease | `caller` |
| A read fails | answers the read with an error; the session goes on (`TestReadFailureDoesNotEndTheSession`) | untouched | the read's |
| A frame is too large | over a pipe, ends the session, answering why (`TestOversizeFrameIsTheCallersFault`); over HTTP, refuses the request (`TestHTTPRefusesAnOversizeBody`) | its lease, over a pipe | `caller` |
| A frame does not decode | over a pipe, ends the session, answering why (`TestUndecodableFrameIsTheCallersFault`); over HTTP, refuses the request (`TestHTTPRefusesWhatItDoesNotSpeak`) | its lease, over a pipe | `caller` |
| EntroQ becomes unreachable | ends the session | its lease | `transient` |
| Operator's first signal | drains every session (`TestDrainFinishesTheTaskInHand`, `TestHTTPDrainWaitsForTheTaskInHand`) | committed as usual | clean |
| Operator's second signal, or the drain outlasts `--lease` | stops at once (`TestShutdownClosesWhatItCannotWaitFor`) | its lease | clean |
| The gateway crashes | nothing: the client sees its pipe close or its POST fail | its lease | none |

## Handling disconnects

The client holds no EntroQ state: the gateway owns the claim, the lease, and
the commit. On any disconnect the client has nothing to roll back, and a task
it held comes back when its lease lapses.

- **Crash-only.** When the gateway exits, let the worker exit too, and let the
  platform restart it: restart on `75`, stay down on `0`, `78` and `70`.
- **Resident.** A worker embedded in a process that must not die on a backend
  blip reconnects in place: start a new session on `transient` with backoff, at
  once on `gone`, and stop on anything else. A new session needs no replay; it
  claims the next task.

Either way, a task may be handed out again after any disconnect, so work and
`success` side effects must be idempotent, or made exactly-once through the
commit: consume the input task in the same modification, and give outputs
deterministic IDs.
