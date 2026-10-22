defmodule EntroQ do
  @moduledoc """
  EntroQ workers in Elixir, through the work gateway (`eqlink work`).

  The gateway runs the EntroQ worker loop in Go -- claim, renew, version
  fix-up, retry, move, doc set claims -- and a worker here answers its small
  JSON protocol (docs/workgateway-protocol.md). A worker supplies functions and
  nothing else:

      alias EntroQ.Modification, as: M

      worker = %EntroQ.Worker{
        queues: ["in"],
        work: fn w, _ctx -> M.of([M.deleting(w.task)]) end
      }

      EntroQ.Gateway.new(entroq: "localhost:37706") |> EntroQ.Gateway.run([worker])

  `work` settles one task: what it returns is committed, and raising
  `EntroQ.RetryError`, `EntroQ.MoveError` or `EntroQ.FatalWorker` gives the
  task another fate. `take` names the doc sets a task needs, `on_success` runs
  after a commit lands, and `on_dependency` runs when a commit loses a
  dependency race.

  Tasks and docs are the protojson of the api/entroq.proto messages, decoded
  with string keys: `w.task["value"]`, `doc["content"]`.
  """

  @doc "A doc set for `take` to claim."
  def claim_set(namespace, key, omit_members \\ false) do
    %{"set" => %{"namespace" => namespace, "key" => key}, "omit_members" => omit_members}
  end
end

defmodule EntroQ.Worker do
  @moduledoc """
  A worker: its functions and how it registers. It runs as one session,
  holding at most one task at a time; for more at once, run several.

  Only `work` is required:

    * `work.(w, ctx)` settles one task. What it returns is committed: an
      `EntroQ.Modification`, or nil to commit nothing.
    * `take.(w, ctx)` names the doc sets the task needs, as a list of
      `EntroQ.claim_set/2`. They are claimed together before `work` runs.
    * `on_success.(w, ctx)` runs after a commit lands, at most once.
    * `on_dependency.(w, deps, ctx)` runs when a commit loses a dependency race.

  Zero takes the gateway's default for each option, and durations are whole
  seconds. `max_claims` zero is five, and negative is no limit; `error_queue`
  is `{inbox}/err` by default.
  """
  @enforce_keys [:queues, :work]
  defstruct [
    :queues,
    :work,
    take: nil,
    on_success: nil,
    on_dependency: nil,
    work_timeout_s: 0,
    max_attempts: 0,
    max_claims: 0,
    retry_delay_s: 0,
    error_queue: ""
  ]

  @doc false
  def config(w) do
    %{
      "queues" => w.queues,
      "protocol_versions" => [1],
      "want_docs" => w.take != nil,
      "want_success" => w.on_success != nil,
      "want_dependency" => w.on_dependency != nil,
      "work_timeout_s" => w.work_timeout_s,
      "max_attempts" => w.max_attempts,
      "max_claims" => w.max_claims,
      "retry_delay_s" => w.retry_delay_s,
      "error_queue" => w.error_queue
    }
  end
end

defmodule EntroQ.Work do
  @moduledoc """
  A task in hand: the task, the docs claimed for it, and the sets they were
  claimed as. `docs` and `sets` are empty in `take`, which decides them.
  """
  defstruct task: nil, docs: [], sets: []

  @doc false
  def new(task, doc_sets \\ []) do
    %__MODULE__{
      task: task,
      sets: Enum.map(doc_sets, & &1["set"]),
      docs: Enum.flat_map(doc_sets, &Map.get(&1, "docs", []))
    }
  end
end

defmodule EntroQ.Modification do
  @moduledoc """
  Task and doc operations committed together, or not at all:

      M.of([M.deleting(w.task), M.inserting(queue: "next", value: v)])

  Name a task or doc as it was handed over. The gateway moves the versions of
  the task in hand and its claimed docs to wherever renewal left them. Fields
  pass through as given, and the gateway refuses what it cannot commit.
  """
  defstruct request: %{}

  def of(ops) do
    request =
      Enum.reduce(ops, %{}, fn {field, item}, acc ->
        Map.update(acc, field, [item], &(&1 ++ [item]))
      end)

    %__MODULE__{request: request}
  end

  @doc """
  A task, given queue and value, and by_ms to delay it; or a doc, given
  namespace and key, and optionally secondary_key and content.
  """
  def inserting(item) do
    item = fields(item)
    {if(Map.has_key?(item, "queue"), do: "inserts", else: "doc_inserts"), item}
  end

  @doc """
  A task or doc with the fields named replaced: a task's queue or value, or
  by_ms to delay it; a doc's content or keys.
  """
  def changing(item, changes \\ []) do
    if doc?(item) do
      data = Map.take(item, ["namespace", "id", "key", "secondaryKey", "content"])
      {"doc_changes", %{"old_id" => doc_id(item), "new_data" => Map.merge(data, fields(changes))}}
    else
      data = %{"queue" => item["queue"], "value" => item["value"]}
      {"changes", %{"old_id" => task_id(item), "new_data" => Map.merge(data, fields(changes))}}
    end
  end

  def deleting(item) do
    if doc?(item), do: {"doc_deletes", doc_id(item)}, else: {"deletes", task_id(item)}
  end

  @doc "Commit only if item is still at its version, changing nothing."
  def depending(item) do
    if doc?(item), do: {"doc_depends", doc_id(item)}, else: {"depends", task_id(item)}
  end

  defp doc?(item), do: Map.has_key?(item, "namespace")
  defp task_id(task), do: Map.take(task, ["queue", "id", "version"])
  defp doc_id(doc), do: Map.take(doc, ["namespace", "id", "version"])
  defp fields(f), do: Map.new(f, fn {k, v} -> {to_string(k), v} end)
end

defmodule EntroQ.Context do
  @moduledoc """
  What a worker function gets besides its work: a way to notice that the
  worker is stopping or to stop it, and reads that answer mid-turn.
  """
  defstruct [:session, :flags]

  @doc """
  Stop this worker once the task in hand is settled. Its fate still lands, and
  its on_success or on_dependency still runs.
  """
  def stop(ctx), do: :atomics.put(hd(ctx.flags), 1, 1)

  @doc "Whether the worker has started stopping."
  def stopping?(ctx), do: stopped?(ctx.flags)

  @doc "Sleep, waking early if the worker starts stopping. True if it did not."
  def sleep(ctx, ms), do: wait(ctx.flags, ms)

  @doc """
  Send a read ("time", "tasks", "docs", "queues", "namespaces") and return its
  answer. A read is not a turn: the task is still in hand.
  """
  def read(ctx, kind, fields \\ %{}) do
    tag = make_ref()
    msg = fields |> Map.new(fn {k, v} -> {to_string(k), v} end) |> Map.put("type", kind)
    send(ctx.session, {:read, self(), tag, msg})

    receive do
      {^tag, {:ok, reply}} -> reply
      {^tag, {:error, e}} -> raise e
    end
  end

  @doc "The backend's clock, in milliseconds since the epoch."
  def time(ctx) do
    case read(ctx, "time")["time_ms"] do
      ms when is_binary(ms) -> String.to_integer(ms)
      ms -> ms
    end
  end

  @doc false
  def stopped?(flags), do: Enum.any?(flags, &(:atomics.get(&1, 1) == 1))

  # Sleep in short slices, so a stop is noticed within one.
  @doc false
  def wait(flags, ms) do
    cond do
      stopped?(flags) ->
        false

      ms <= 0 ->
        true

      true ->
        Process.sleep(min(ms, 50))
        wait(flags, ms - 50)
    end
  end
end

defmodule EntroQ.RetryError do
  @moduledoc """
  Raise to try the task again later, counting the attempt. Once the attempts
  run out it moves to `or_move_to`, or to the worker's error queue.
  """
  defexception message: "", or_move_to: ""
end

defmodule EntroQ.MoveError do
  @moduledoc "Raise to move the task now, to `queue` or to the worker's error queue."
  defexception message: "", queue: ""
end

defmodule EntroQ.FatalWorker do
  @moduledoc """
  Raise to stop every worker with this error, for something no retry can fix
  in this process. The task comes back when its lease lapses.
  """
  defexception message: ""
end

defmodule EntroQ.GatewayError do
  @moduledoc """
  The gateway ended the session. `class` says whether a new one could help:
  "transient" is worth retrying; "caller" and "gateway" are not.
  """
  defexception class: "gateway", message: ""

  @doc false
  def new(class, message), do: %__MODULE__{class: class, message: "[#{class}] #{message}"}
end

defmodule EntroQ.SessionGone do
  @moduledoc false
  defexception message: ""
end
