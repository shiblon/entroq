defmodule EntroQ.Gateway do
  @moduledoc """
  Runs workers through `eqlink work`, each as a session of its own, and stops
  them together.

      gateway = EntroQ.Gateway.new(entroq: "localhost:37706")
      :ok = EntroQ.Gateway.run(gateway, [worker_a, worker_b])

  By default it starts one `eqlink work` child, shared by every session, and
  stops it when they are done. Options:

    * `:entroq` - the EntroQ service the child connects to.
    * `:eqlink` - the eqlink binary; "eqlink" on PATH by default.
    * `:args` - the operator's flags for `eqlink work`, such as ["--lease", "10s"].
    * `:url` - the URL of an `eqlink work --addr` already serving, instead of
      a child.
    * `:log` - where the gateway's notes go; stderr by default.
  """

  alias EntroQ.{Context, GatewayError, Session, SessionGone}

  defstruct [:connect, :child, :stopping, :log]

  def new(opts \\ []) do
    gw = %__MODULE__{stopping: :atomics.new(1, []), log: opts[:log] || (&IO.puts(:stderr, &1))}

    if url = opts[:url] do
      %{gw | connect: fn -> exchange(url) end}
    else
      argv = [opts[:eqlink] || "eqlink", "--entroq", opts[:entroq] || "localhost:37706", "work"]
      {:ok, child} = EntroQ.Child.start_link(argv ++ (opts[:args] || []))
      %{gw | child: child, connect: fn -> exchange(EntroQ.Child.url(child)) end}
    end
  end

  @doc """
  Run until every worker has stopped: by `EntroQ.Context.stop/1`, by `stop/1`,
  or by a failure a new session cannot fix, which is then returned as
  `{:error, e}`.
  """
  def run(%__MODULE__{} = gw, workers) do
    workers
    |> Enum.map(fn w -> Task.async(fn -> serve(gw, w, 0) end) end)
    |> Task.await_many(:infinity)
    |> Enum.find(:ok, &(&1 != :ok))
  after
    if gw.child, do: EntroQ.Child.close(gw.child)
  end

  @doc """
  Drain: every task in hand finishes, and no session claims again. A child
  gateway is told by signal, and answers each session with quit.
  """
  def stop(%__MODULE__{} = gw, signal \\ "TERM") do
    :atomics.put(gw.stopping, 1, 1)
    if gw.child, do: EntroQ.Child.signal(gw.child, signal)
    :ok
  end

  # Run one worker's sessions, one after another, until it stops. A session
  # that is gone is replaced at once. A transient error, or a gateway that went
  # away or never answered, is retried with backoff. Anything else stops every
  # worker, and run returns it.
  defp serve(gw, worker, backoff_ms) do
    flags = [:atomics.new(1, []), gw.stopping]

    next =
      try do
        Session.run(worker, gw.connect.(), flags)
        {:next, 0}
      rescue
        SessionGone ->
          {:next, 0}

        e ->
          if match?(%GatewayError{class: "transient"}, e) do
            gw.log.("[worker] #{Exception.message(e)}; reconnecting")
            {:next, min(max(backoff_ms * 2, 200), 5000)}
          else
            gw.log.("[worker] #{Exception.message(e)}; stopping")
            stop(gw)
            {:error, e}
          end
      end

    case next do
      {:error, e} -> {:error, e}
      {:next, ms} -> if Context.wait(flags, ms), do: serve(gw, worker, ms), else: :ok
    end
  end

  # One session's exchange over HTTP: a POST per message.
  defp exchange({:ok, url}), do: exchange(url)
  defp exchange({:error, e}), do: raise(e)

  defp exchange(url) when is_binary(url) do
    url = String.to_charlist(url)

    fn msg ->
      request = {url, [], ~c"application/json", JSON.encode!(msg)}

      case :httpc.request(:post, request, [], body_format: :binary) do
        {:ok, {{_, status, _}, headers, body}} ->
          if not json?(headers) do
            # Not the gateway's answer (a proxy's, say), so the status is all
            # there is to go on.
            raise GatewayError.new(
                    if(status < 500, do: "caller", else: "gateway"),
                    "HTTP #{status}: #{String.trim(body)}"
                  )
          end

          case JSON.decode!(body) do
            %{"type" => "error"} = reply -> raise failure(reply)
            reply -> reply
          end

        {:error, reason} ->
          # A gateway that went away, or never answered.
          raise GatewayError.new("transient", "gateway unreachable: #{inspect(reason)}")
      end
    end
  end

  defp json?(headers) do
    case List.keyfind(headers, ~c"content-type", 0) do
      {_, type} -> String.starts_with?(to_string(type), "application/json")
      nil -> false
    end
  end

  @doc false
  # The exception an error message raises: "gone" starts a new session.
  def failure(%{"class" => "gone"} = msg), do: %SessionGone{message: msg["message"] || ""}
  def failure(msg), do: GatewayError.new(msg["class"] || "gateway", msg["message"] || "")
end

defmodule EntroQ.Child do
  @moduledoc false
  # One `eqlink work --addr` child, shared by every session of a gateway,
  # started when a session first needs it and again if it exits. It exits on
  # its own when this VM does.

  use GenServer

  alias EntroQ.{GatewayError, SessionGone}

  def start_link(argv), do: GenServer.start_link(__MODULE__, argv)

  @doc "The child's URL, starting it if it is not running: {:ok, url} or {:error, e}."
  def url(child), do: GenServer.call(child, :url, :infinity)

  def signal(child, signal), do: GenServer.call(child, {:signal, signal})

  @doc "Stop the child, once every session has ended."
  def close(child), do: GenServer.call(child, :close, :infinity)

  @impl true
  def init(argv), do: {:ok, %{argv: argv, port: nil, url: nil}}

  @impl true
  def handle_call(:url, _from, %{port: nil} = st) do
    case start(st.argv) do
      {:ok, port, url} -> {:reply, {:ok, url}, %{st | port: port, url: url}}
      {:error, e} -> {:reply, {:error, e}, st}
    end
  end

  def handle_call(:url, _from, st), do: {:reply, {:ok, st.url}, st}

  def handle_call({:signal, _}, _from, %{port: nil} = st), do: {:reply, :ok, st}

  def handle_call({:signal, signal}, _from, st) do
    case Port.info(st.port, :os_pid) do
      {:os_pid, pid} -> System.cmd("kill", ["-#{signal}", to_string(pid)], stderr_to_stdout: true)
      nil -> :ok
    end

    {:reply, :ok, st}
  end

  def handle_call(:close, _from, %{port: nil} = st), do: {:reply, :ok, st}

  def handle_call(:close, from, st) do
    {:reply, :ok, st} = handle_call({:signal, "TERM"}, from, st)
    port = st.port

    receive do
      {^port, {:exit_status, _}} -> :ok
    end

    {:reply, :ok, %{st | port: nil}}
  end

  @impl true
  def handle_info({port, {:exit_status, _}}, %{port: port} = st),
    do: {:noreply, %{st | port: nil}}

  def handle_info(_, st), do: {:noreply, st}

  defp start([cmd | args]) do
    case System.find_executable(cmd) do
      nil -> {:error, GatewayError.new("caller", "start #{cmd}: not found")}
      exe -> announce(exe, args)
    end
  end

  # Start the child and read the one line it writes once listening, or the
  # error that stopped it. No --parent-pid: a port's parent is the VM's
  # erl_child_setup, not the VM, and the child watches the parent it started
  # with, which exits with the VM.
  defp announce(exe, args) do
    args = args ++ ["--addr", "127.0.0.1:0"]

    port =
      Port.open({:spawn_executable, exe}, [:binary, :exit_status, {:line, 65_536}, args: args])

    receive do
      {^port, {:data, {:eol, line}}} ->
        case JSON.decode(line) do
          {:ok, %{"type" => "error"} = msg} -> {:error, EntroQ.Gateway.failure(msg)}
          {:ok, %{"addr" => addr}} -> {:ok, port, "http://#{addr}/"}
          _ -> {:error, GatewayError.new("gateway", "gateway announced #{inspect(line)}")}
        end

      {^port, {:exit_status, code}} ->
        {:error, exit_error(code)}
    end
  end

  # What a gateway's exit code says, when it exited without a word.
  defp exit_error(0), do: %SessionGone{message: "gateway exited"}
  defp exit_error(75), do: GatewayError.new("transient", "gateway exited 75")
  defp exit_error(78), do: GatewayError.new("caller", "gateway exited 78")
  defp exit_error(code), do: GatewayError.new("gateway", "gateway exited #{code}")
end
