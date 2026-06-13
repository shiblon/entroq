# EntroQ Elixir Client

Elixir client for the [EntroQ](https://github.com/shiblon/entroq) task queue.

This package talks to EntroQ over the HTTP/JSON RPC API. The worker API follows
the same safety model as the Go `DoModify` path: handler code returns intent,
and the worker owns renewal, final version capture, version fix-up, and the
final atomic `Modify`.

## Installation

The package is not published yet. From this repository:

```elixir
def deps do
  [
    {:entroq, path: "clients/elixir"}
  ]
end
```

## Client

```elixir
client = EntroQ.new("http://localhost:9100")
```

Client functions return `{:ok, value}` or `{:error, reason}`. Dependency
conflicts are returned as `%EntroQ.DependencyError{}`.

### Time

```elixir
{:ok, time_ms} = EntroQ.Client.time(client)
```

### Queue Stats

```elixir
{:ok, queues} = EntroQ.Client.queues(client, prefix: "/jobs")

Enum.each(queues, fn q ->
  IO.inspect({q.name, q.num_available, q.num_claimed, q.num_tasks})
end)
```

### Reading Tasks

```elixir
{:ok, tasks} =
  EntroQ.Client.tasks(client,
    queue: "/jobs/email",
    limit: 100,
    omit_values: false
  )
```

### Raw Claim

`try_claim/3` returns immediately with a task or `nil`.

```elixir
case EntroQ.Client.try_claim(client, ["/jobs/email", "/jobs/sms"]) do
  {:ok, nil} ->
    :empty

  {:ok, task} ->
    IO.inspect(task.value)

  {:error, reason} ->
    raise inspect(reason)
end
```

`claim/3` uses the server-side wait endpoint.

```elixir
{:ok, task} =
  EntroQ.Client.claim(client, "/jobs/email",
    duration_ms: 30_000,
    poll_ms: 5_000
  )
```

### Raw Modify

`Modify` is the atomic commit primitive. You can insert, change, delete, or
depend on tasks and docs in one request.

```elixir
mod =
  EntroQ.Modification.new()
  |> EntroQ.Modification.insert(%EntroQ.TaskData{
    queue: "/jobs/email",
    value: %{"to" => "chris@example.com", "template" => "welcome"}
  })

{:ok, result} = EntroQ.Client.modify(client, mod)
```

Finish a claimed task by deleting the exact version you claimed:

```elixir
{:ok, task} = EntroQ.Client.try_claim(client, "/jobs/email")

mod =
  EntroQ.Modification.new()
  |> EntroQ.Modification.delete(task)

{:ok, _result} = EntroQ.Client.modify(client, mod)
```

Move a task and update its error metadata atomically:

```elixir
mod =
  EntroQ.Modification.change(task,
    queue: "/jobs/email/err",
    at_ms: 0,
    attempt: task.attempt + 1,
    err: "invalid recipient"
  )

{:ok, _result} = EntroQ.Client.modify(client, mod)
```

### Reading Docs

Docs are durable shared state in the same transaction space as tasks.

```elixir
{:ok, docs} =
  EntroQ.Client.docs(client,
    namespace: "/config",
    key_start: "/jobs/",
    limit: 100
  )
```

Read exact doc IDs when you already know them:

```elixir
{:ok, docs} =
  EntroQ.Client.docs(client,
    namespace: "/config",
    ids: ["doc-id-1", "doc-id-2"]
  )
```

### Claiming Docs

`claim_docs/2` atomically claims all docs sharing a namespace and primary key.

```elixir
{:ok, docs} =
  EntroQ.Client.claim_docs(client,
    namespace: "/config",
    key: "/jobs/email",
    duration_ms: 30_000
  )
```

### Doc Namespace Stats

```elixir
{:ok, namespaces} = EntroQ.Client.namespace_stats(client, prefix: "/config")

Enum.each(namespaces, fn ns ->
  IO.inspect({ns.name, ns.num_docs, ns.num_claimed})
end)
```

## Worker

```elixir
defmodule MyWorker do
  use EntroQ.Worker

  @impl true
  def take_docs(task) do
    [
      EntroQ.DocClaim.new("/config", task.queue <> "/settings")
    ]
  end

  @impl true
  def perform(task, docs) do
    {:modify,
     EntroQ.Modification.new()
     |> EntroQ.Modification.delete(task)
     |> EntroQ.Modification.change(List.first(docs), content: %{"seen" => true})}
  end
end

client = EntroQ.new("http://localhost:9100")
EntroQ.Worker.run(client, ["/my/queue"], MyWorker)
```

For small applications, use the function form:

```elixir
EntroQ.Worker.run(client, ["/my/queue"],
  perform: fn task, _docs ->
    {:modify, EntroQ.Modification.delete(task)}
  end
)
```

`perform/2` receives the originally claimed task/docs. While it runs, the worker
renews the task and claimed docs together in one atomic `Modify`. When
`perform/2` returns, renewal stops and any in-flight renewal finishes before the
worker commits the result.

Supported `perform/2` results:

- `{:modify, modification}`: stop renewal, fix versions, apply the modification.
- `{:finish, payload}`: stop renewal, then call `finish(final_task, final_docs, payload)`.
- `:ok`: work succeeded; no default modification is applied.
- `{:retry, reason}` or `{:retry, reason, opts}`: requeue with attempt increment.
- `{:move, reason}` or `{:move, reason, opts}`: move to the error queue.
- `:stop`: stop the worker loop.
- `{:error, reason}`: stop with an error.

`finish/3` receives stable final versions after renewal has stopped. It may
return the same result shapes as `perform/2`.

## Development

Use the Elixir container from the repository root:

```bash
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp \
  -v "$PWD:/repo" -w /repo/clients/elixir \
  elixir:1.17 sh -lc 'mix local.hex --force && mix test'
```
