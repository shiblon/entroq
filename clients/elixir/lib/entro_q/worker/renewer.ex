defmodule EntroQ.Worker.Renewer do
  @moduledoc false

  require Logger

  def start_link(client, task, docs, lease_ms, owner) do
    pid =
      spawn_link(fn ->
        loop(%{
          client: client,
          task: task,
          docs: docs,
          lease_ms: lease_ms,
          owner: owner,
          error: nil
        })
      end)

    {:ok, pid}
  end

  def stop(pid) do
    ref = make_ref()
    send(pid, {:stop, self(), ref})

    receive do
      {:stopped, ^ref, task, docs, error} -> {task, docs, error}
    end
  end

  defp loop(state) do
    receive do
      {:stop, from, ref} ->
        send(from, {:stopped, ref, state.task, state.docs, state.error})
    after
      max(div(state.lease_ms, 2), 1) ->
        renew(state)
    end
  end

  defp renew(%{error: nil} = state) do
    case EntroQ.Worker.renew(state.client, state.task, state.docs, state.lease_ms) do
      {:ok, task, docs} ->
        loop(%{state | task: task, docs: docs})

      {:error, %EntroQ.DependencyError{} = error} ->
        send(state.owner, {:renewal_failed, error})
        loop(%{state | error: error})

      {:error, reason} ->
        Logger.warning("EntroQ renewal failed transiently: #{inspect(reason)}")
        loop(state)
    end
  end

  defp renew(state), do: loop(state)
end
