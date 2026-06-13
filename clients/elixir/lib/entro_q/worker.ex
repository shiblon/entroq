defmodule EntroQ.Worker do
  @moduledoc """
  Race-safe EntroQ worker loop.

  Worker handlers return intent from `perform/2`. The worker renews the claimed
  task and any claimed docs while `perform/2` runs, stops renewal before commit,
  captures stable final versions, fixes returned modifications, and applies one
  atomic `Modify`.
  """

  alias EntroQ.{DependencyError, DocClaim, Modification}
  alias EntroQ.Worker.{Handler, Session}

  @type result ::
          {:modify, Modification.t()}
          | {:finish, term()}
          | :ok
          | {:retry, term()}
          | {:retry, term(), keyword()}
          | {:move, term()}
          | {:move, term(), keyword()}
          | :stop
          | {:error, term()}

  @callback take_docs(EntroQ.Task.t()) ::
              [DocClaim.t()] | {:ok, [DocClaim.t()]} | {:error, term()}
  @callback perform(EntroQ.Task.t(), [EntroQ.Doc.t()]) :: result()
  @callback finish(EntroQ.Task.t(), [EntroQ.Doc.t()], term()) :: result()

  defmacro __using__(_opts) do
    quote do
      @behaviour EntroQ.Worker

      @impl true
      def take_docs(_task), do: []

      @impl true
      def finish(_task, _docs, _payload), do: :ok

      defoverridable take_docs: 1, finish: 3
    end
  end

  @doc """
  Runs a worker loop until the handler stops or an unrecoverable error occurs.
  """
  def run(client, queues, handler_or_opts, opts \\ []) do
    handler = Handler.new(handler_or_opts)
    queues = List.wrap(queues)

    Stream.repeatedly(fn -> run_once(client, queues, handler, opts) end)
    |> Enum.reduce_while(:ok, fn
      :ok, _acc -> {:cont, :ok}
      {:ok, _result}, _acc -> {:cont, :ok}
      :stop, _acc -> {:halt, :ok}
      {:error, reason}, _acc -> {:halt, {:error, reason}}
    end)
  end

  @doc """
  Claims and processes a single task.

  This is mainly useful for tests, controlled worker processes, and examples.
  """
  def run_once(client, queues, handler_or_opts, opts \\ []) do
    handler = Handler.new(handler_or_opts)

    with {:ok, task} <-
           call_client(client, :claim, [
             List.wrap(queues),
             [duration_ms: lease_ms(opts), poll_ms: poll_ms(opts)]
           ]) do
      process(client, task, handler, opts)
    end
  end

  @doc false
  def process(client, task, handler_or_opts, opts \\ []) do
    handler = Handler.new(handler_or_opts)

    if max_attempts_exceeded?(task, opts) do
      move_task(
        client,
        task,
        [],
        "max attempts (#{Keyword.get(opts, :max_attempts)}) exceeded",
        [],
        opts
      )
    else
      with {:ok, docs} <- claim_docs(client, task, handler, opts) do
        Session.run(client, task, docs, handler, opts)
      end
    end
  end

  @doc false
  def finish_result(client, result, final_task, final_docs, handler, opts) do
    case normalize_result(result) do
      {:modify, %Modification{} = modification} ->
        modification = Modification.fix_versions(modification, final_task, final_docs)
        call_client(client, :modify, [modification, []])

      {:finish, payload} ->
        case Handler.finish(handler, final_task, final_docs, payload) do
          {:modify, %Modification{} = modification} ->
            modification = Modification.fix_versions(modification, final_task, final_docs)
            call_client(client, :modify, [modification, []])

          other ->
            finish_result(client, other, final_task, final_docs, handler, opts)
        end

      :ok ->
        :ok

      {:retry, reason, retry_opts} ->
        retry_task(client, final_task, final_docs, reason, retry_opts, opts)

      {:move, reason, move_opts} ->
        move_task(client, final_task, final_docs, reason, move_opts, opts)

      :stop ->
        :stop

      {:error, reason} ->
        {:error, reason}
    end
  end

  @doc false
  def renew(client, task, docs, lease_ms) do
    at_ms = System.system_time(:millisecond) + lease_ms

    modification =
      Modification.new()
      |> Modification.change(task, at_ms: at_ms)
      |> then(fn mod ->
        Enum.reduce(docs, mod, fn doc, acc -> Modification.change(acc, doc, at_ms: at_ms) end)
      end)

    with {:ok, result} <- call_client(client, :modify, [modification, []]) do
      case {result.tasks_changed, result.docs_changed} do
        {[changed_task], changed_docs} when length(changed_docs) == length(docs) ->
          {:ok, changed_task, changed_docs}

        _other ->
          {:error, :renewal_result_mismatch}
      end
    end
  end

  @doc false
  def call_client(client, fun, args) do
    apply(client.__struct__, fun, [client | args])
  end

  defp claim_docs(client, task, handler, opts) do
    with {:ok, claims} <- Handler.take_docs(handler, task) do
      claims
      |> Enum.map(&DocClaim.new/1)
      |> Enum.sort_by(fn claim -> {claim.namespace, claim.key} end)
      |> Enum.reduce_while({:ok, []}, fn claim, {:ok, docs} ->
        claim = %{claim | duration_ms: claim.duration_ms || lease_ms(opts)}

        case call_client(client, :claim_docs, [claim]) do
          {:ok, claimed} -> {:cont, {:ok, docs ++ claimed}}
          {:error, %DependencyError{} = error} -> {:halt, {:error, error}}
          {:error, reason} -> {:halt, {:error, reason}}
        end
      end)
    end
  end

  defp retry_task(client, task, docs, reason, retry_opts, opts) do
    attempt = task.attempt + 1
    max_attempts = Keyword.get(opts, :max_attempts, 0)

    if max_attempts > 0 and attempt >= max_attempts do
      move_task(client, task, docs, reason, retry_opts, opts)
    else
      delay_ms = Keyword.get(retry_opts, :delay_ms, Keyword.get(opts, :retry_delay_ms, 30_000))
      at_ms = System.system_time(:millisecond) + delay_ms

      modification =
        Modification.new()
        |> Modification.change(task, at_ms: at_ms, attempt: attempt, err: reason_string(reason))
        |> release_docs(docs)

      call_client(client, :modify, [modification, []])
    end
  end

  defp move_task(client, task, docs, reason, move_opts, opts) do
    queue = Keyword.get(move_opts, :queue) || error_queue(task, opts)

    modification =
      Modification.new()
      |> Modification.change(task, queue: queue, at_ms: 0, err: reason_string(reason))
      |> release_docs(docs)

    call_client(client, :modify, [modification, []])
  end

  defp release_docs(modification, docs) do
    Enum.reduce(docs, modification, fn doc, acc -> Modification.change(acc, doc, at_ms: 0) end)
  end

  defp error_queue(task, opts) do
    case Keyword.get(opts, :error_queue) do
      nil -> task.queue <> "/err"
      queue when is_binary(queue) -> queue
      fun when is_function(fun, 1) -> fun.(task)
    end
  end

  defp normalize_result({:retry, reason}), do: {:retry, reason, []}
  defp normalize_result({:move, reason}), do: {:move, reason, []}
  defp normalize_result(other), do: other

  defp max_attempts_exceeded?(task, opts) do
    max_attempts = Keyword.get(opts, :max_attempts, 0)
    max_attempts > 0 and task.attempt >= max_attempts
  end

  defp lease_ms(opts), do: Keyword.get(opts, :lease_ms, 30_000)
  defp poll_ms(opts), do: Keyword.get(opts, :poll_ms, 5_000)

  defp reason_string(reason) when is_binary(reason), do: reason
  defp reason_string(reason), do: inspect(reason)
end
