defmodule EntroQ.Worker.Session do
  @moduledoc false

  alias EntroQ.Worker.{Handler, Renewer}

  def run(client, task, docs, handler, opts) do
    lease_ms = Keyword.get(opts, :lease_ms, 30_000)
    {:ok, renewer} = Renewer.start_link(client, task, docs, lease_ms, self())
    perform_task = Task.async(fn -> safe_perform(handler, task, docs) end)

    receive do
      {ref, {:ok, result}} when ref == perform_task.ref ->
        Task.shutdown(perform_task, :brutal_kill)
        {final_task, final_docs, renewal_error} = Renewer.stop(renewer)

        if renewal_error do
          {:error, renewal_error}
        else
          EntroQ.Worker.finish_result(client, result, final_task, final_docs, handler, opts)
        end

      {ref, {:error, reason}} when ref == perform_task.ref ->
        Task.shutdown(perform_task, :brutal_kill)
        {_final_task, _final_docs, _renewal_error} = Renewer.stop(renewer)
        {:error, reason}

      {:renewal_failed, reason} ->
        Task.shutdown(perform_task, :brutal_kill)
        {_final_task, _final_docs, _renewal_error} = Renewer.stop(renewer)
        {:error, reason}
    end
  end

  defp safe_perform(handler, task, docs) do
    {:ok, Handler.perform(handler, task, docs)}
  rescue
    exception ->
      {:error, {exception, __STACKTRACE__}}
  catch
    kind, reason ->
      {:error, {kind, reason}}
  end
end
