# EntroQ Elixir Client

Workers for the [EntroQ](https://github.com/shiblon/entroq) task queue, written
in Elixir and run through the work gateway, `eqlink work`.

The gateway runs the EntroQ worker loop in Go: it claims tasks, renews them,
claims doc sets, fixes up versions at commit, and retries, moves, and backs off.
A worker here answers its small JSON protocol over HTTP and holds no EntroQ
state of its own, so it has nothing to get wrong about leases or claims. The
protocol is [`docs/workgateway-protocol.md`](../../docs/workgateway-protocol.md).

## Installation

The package is not published yet. From this repository:

```elixir
def deps do
  [
    {:entroq, path: "clients/elixir"}
  ]
end
```

It has no runtime dependencies. It needs Elixir 1.18 or later, and `eqlink` on
`PATH` (or named with `:eqlink`).

## Quick start

```elixir
alias EntroQ.Modification, as: M

worker = %EntroQ.Worker{
  queues: ["/my/queue"],
  work: fn w, _ctx ->
    IO.inspect(w.task["value"], label: "Processing")
    # Commit atomically: here, delete the task to finish it.
    M.of([M.deleting(w.task)])
  end
}

:ok = EntroQ.Gateway.new(entroq: "localhost:37706") |> EntroQ.Gateway.run([worker])
```

The gateway starts one `eqlink work` child, shared by every worker, and stops it
when they are done. `EntroQ.Gateway.stop/1` drains: every task in hand
finishes and nothing more is claimed. The child exits on its own if the VM does.

## Workers

A `%EntroQ.Worker{}` is one session with the gateway, holding at most one task
at a time. For more at once, or for several kinds of work, run several:

```elixir
EntroQ.Gateway.run(gateway, [worker_a | List.duplicate(worker_b, 4)])
```

Each session claims as a consumer of its own, so workers never hold each
other's doc sets.

### The functions

Only `work` is required.

- `work: fn w, ctx -> ... end` settles one task. What it returns is committed:
  an `EntroQ.Modification`, or nil to commit nothing. The task in hand is not
  deleted unless the modification says so.
- `take: fn w, ctx -> ... end` names the doc sets the task needs, with
  `EntroQ.claim_set(ns, key)`. They are claimed together, all or none, before
  `work` runs, and `w.docs` holds their members. A set someone else holds sends
  the task back with backoff.
- `on_success: fn w, ctx -> ... end` runs after a commit lands. It is at most
  once, so what it does must be safe to skip.
- `on_dependency: fn w, deps, ctx -> ... end` runs when a commit loses a
  dependency race.

Raise instead of returning to give the task another fate:

- `raise EntroQ.RetryError, message: "...", or_move_to: "..."` tries it again
  later, counting the attempt; once the attempts run out it moves to
  `or_move_to` or the error queue.
- `raise EntroQ.MoveError, message: "...", queue: "..."` moves it now, to
  `queue` or the error queue.
- `raise EntroQ.FatalWorker, message: "..."` stops every worker; the task comes
  back when its lease lapses.
- Anything else stops this worker, after the gateway quarantines the task if
  this was its last allowed claim.

`w` carries `task`, `docs`, and `sets`, the protojson the gateway sent, with
string keys: `w.task["value"]`, `doc["content"]`. `ctx` works with
`EntroQ.Context`: `stopping?/1`, true once the worker starts stopping;
`stop/1`, to stop once this task is settled; `sleep/2`, which wakes early when
stopping; and `read/3` and `time/1`, reads the gateway answers mid-turn.

Long work needs nothing extra: the client tells the gateway it is still working
while a function runs.

### Modifications

`M.of([...])` commits its operations together, or not at all:

```elixir
M.of([
  M.deleting(w.task),
  M.inserting(queue: "/next", value: %{n: 1}),
  M.inserting(queue: "/later", value: 2, by_ms: 60_000),
  M.changing(doc, content: %{count: 3}),
  M.depending(other_doc)
])
```

Name a task or doc as it was handed over; the gateway moves the versions of the
task in hand and its claimed docs to wherever renewal left them.

### Options

```elixir
%EntroQ.Worker{
  queues: queues,
  work: work,
  work_timeout_s: 300, # how long one task should take; the operator caps it
  max_attempts: 0,     # retries before quarantine; 0 is no limit
  max_claims: 0,       # claims a task may fail to survive; 0 is 5, negative is none
  retry_delay_s: 0,    # base retry delay, and the wait after doc contention; 0 is 30s
  error_queue: ""      # where moved tasks go; "{inbox}/err" by default
}
```

Durations are whole seconds.

## A gateway you did not start

Given the URL of an `eqlink work --addr` already serving:

```elixir
gateway = EntroQ.Gateway.new(url: "http://127.0.0.1:43117/")
```

## Development

```bash
mix test   # the integration tests build eqlink and eqmem with Go, if it is installed
```
