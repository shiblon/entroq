defmodule EntroQ.Session do
  @moduledoc false
  # One session: the protocol loop and its switch. The session process owns
  # the exchange, so the reads and keepalives it serves while a function runs
  # never overlap the answer it is waiting for.

  alias EntroQ.{Context, GatewayError, Modification, SessionGone, Work}

  defstruct [:worker, :exchange, :ctx, id: "", keepalive_ms: :infinity]

  @doc """
  Run one session until the gateway says quit. exchange sends a message and
  returns the gateway's answer, raising GatewayError or SessionGone; flags are
  the session's own stop flag and the gateway's.
  """
  def run(worker, exchange, flags) do
    s = %__MODULE__{
      worker: worker,
      exchange: exchange,
      ctx: %Context{session: self(), flags: flags}
    }

    reply = send_msg(s, %{"type" => "config", "config" => EntroQ.Worker.config(worker)})
    s = %{s | id: reply["session"] || ""}

    s =
      case reply do
        # Every third of the window, so one late keepalive is not fatal.
        %{"type" => "ready", "silence_timeout_s" => silence} when silence > 0 ->
          %{s | keepalive_ms: round(silence * 1000 / 3)}

        _ ->
          s
      end

    loop(s, reply, nil)
  end

  defp loop(_s, %{"type" => "quit"}, _work), do: :ok

  defp loop(s, reply, work) do
    w = s.worker

    {msg, work} =
      case reply["type"] do
        "ready" ->
          # The gateway is waiting on a claim, and says so now and then.
          {%{"type" => "ready"}, work}

        "take" ->
          work = Work.new(reply["task"])
          {claims, fate} = settle(s, w.take, [work, s.ctx])
          {Map.merge(%{"type" => "take", "doc_set_claims" => claims || []}, fate), work}

        "work" ->
          work = Work.new(reply["task"], reply["doc_sets"] || [])
          {mod, fate} = settle(s, w.work, [work, s.ctx])
          msg = Map.merge(%{"type" => "modify"}, fate)

          case mod do
            %Modification{request: request} -> {Map.put(msg, "modification", request), work}
            _ -> {msg, work}
          end

        "success" ->
          {_, fate} = settle(s, w.on_success, [work, s.ctx])
          {Map.put(fate, "type", "done"), work}

        "dependency" ->
          {_, fate} = settle(s, w.on_dependency, [work, reply["deps"] || [], s.ctx])
          {Map.put(fate, "type", "done"), work}

        other ->
          raise GatewayError.new("caller", "unexpected #{inspect(other)}")
      end

    loop(s, send_msg(s, msg), work)
  end

  # The loop's exchange. Once stopping, every message carries quit.
  defp send_msg(s, msg) do
    msg = Map.put(msg, "session", s.id)
    msg = if Context.stopped?(s.ctx.flags), do: Map.put(msg, "quit", true), else: msg
    s.exchange.(msg)
  end

  # Run fun, returning what it returns and, if it raised, the fate that asks
  # for: {result, fate}.
  defp settle(_s, nil, _args), do: {nil, %{}}

  defp settle(s, fun, args) do
    case await(s, Task.async(fn -> attempt(fun, args) end)) do
      {:ok, result} -> {result, %{}}
      {:fate, fate} -> {nil, fate}
      # A read that found the session ending; the task has no say.
      {:session, e} -> raise e
    end
  end

  defp attempt(fun, args) do
    {:ok, apply(fun, args)}
  rescue
    e in [GatewayError, SessionGone] -> {:session, e}
    e in EntroQ.RetryError -> {:fate, fate("retry", e.message, e.or_move_to)}
    e in EntroQ.MoveError -> {:fate, fate("move", e.message, e.queue)}
    e in EntroQ.FatalWorker -> {:fate, fate("fatal", e.message)}
    # A failure the function did not classify stops the worker, after the
    # gateway quarantines the task if this was its last claim.
    e -> {:fate, fate("error", "#{inspect(e.__struct__)}: #{Exception.message(e)}")}
  catch
    kind, reason -> {:fate, fate("error", Exception.format_banner(kind, reason))}
  end

  defp fate(outcome, message, queue \\ nil) do
    error = if queue, do: %{"message" => message, "queue" => queue}, else: %{"message" => message}
    %{"outcome" => outcome, "error" => error}
  end

  # Wait for the function, serving its reads, and telling the gateway the
  # client is still working if it runs long. Short work sends nothing.
  defp await(s, %Task{ref: ref} = task) do
    receive do
      {^ref, result} ->
        Process.demonitor(ref, [:flush])
        result

      {:read, from, tag, msg} ->
        send(from, {tag, try_send(s, msg)})
        await(s, task)
    after
      s.keepalive_ms ->
        # A failure means the session is ending; the answer will say so.
        try_send(s, %{"type" => "working"})
        await(s, task)
    end
  end

  defp try_send(s, msg) do
    {:ok, send_msg(s, msg)}
  rescue
    e -> {:error, e}
  end
end
