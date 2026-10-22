defmodule EntroQ.IntegrationTest do
  # A real eqlink work, as a child of each Gateway, against eqmem, both built
  # from this repository. Skips if Go is absent.
  use ExUnit.Case

  alias EntroQ.{Context, Gateway, Worker}
  alias EntroQ.Modification, as: M

  @moduletag timeout: 180_000
  if System.find_executable("go") == nil, do: @moduletag(skip: "needs go")

  @repo Path.expand("../../..", __DIR__)

  setup_all do
    bin = Path.join(System.tmp_dir!(), "entroq-ex-#{System.unique_integer([:positive])}")
    File.mkdir_p!(bin)
    on_exit(fn -> File.rm_rf!(bin) end)

    for cmd <- ["eqlink", "eqmem"] do
      {_, 0} =
        System.cmd("go", ["build", "-o", Path.join(bin, cmd), "./cmd/#{cmd}"],
          cd: @repo,
          stderr_to_stdout: true
        )
    end

    [http, grpc] = [free_port(), free_port()]
    eqmem = Path.join(bin, "eqmem")

    port =
      Port.open({:spawn_executable, eqmem}, [
        :binary,
        args: ["serve", "--http_port", "#{http}", "--port", "#{grpc}"]
      ])

    {:os_pid, pid} = Port.info(port, :os_pid)
    on_exit(fn -> System.cmd("kill", ["#{pid}"]) end)

    base = "http://127.0.0.1:#{http}"
    wait_until_serving(base, 100)
    %{base: base, entroq: "127.0.0.1:#{grpc}", eqlink: Path.join(bin, "eqlink")}
  end

  test "works every task once, each session holding the doc set alone", ctx do
    prefix = "ex-#{System.unique_integer([:positive])}"
    {queue, ns, n} = {"#{prefix}/q", "#{prefix}/ns", 8}

    rest(ctx.base, :post, "/api/v0/modify", %{
      claimantId: "seed",
      inserts: for(i <- 0..(n - 1), do: %{queue: queue, value: i}),
      docInserts: [%{namespace: ns, key: "count", content: 0}]
    })

    gw = Gateway.new(entroq: ctx.entroq, eqlink: ctx.eqlink, log: fn _ -> nil end)
    {:ok, seen} = Agent.start_link(fn -> %{values: [], inside: 0, overlap: 0} end)

    worker = %Worker{
      queues: [queue],
      take: fn _w, _ctx -> [EntroQ.claim_set(ns, "count")] end,
      work: fn w, _ctx ->
        Agent.update(seen, fn s ->
          %{s | inside: s.inside + 1, overlap: s.overlap + min(s.inside, 1)}
        end)

        Process.sleep(5)
        Agent.update(seen, fn s -> %{s | inside: s.inside - 1} end)
        [count] = w.docs
        M.of([M.deleting(w.task), M.changing(count, content: count["content"] + 1)])
      end,
      on_success: fn w, _ctx ->
        values =
          Agent.get_and_update(seen, fn s ->
            {[w.task["value"] | s.values], %{s | values: [w.task["value"] | s.values]}}
          end)

        if length(values) == n, do: Gateway.stop(gw)
      end,
      # A session that finds the set held retries after this, not the
      # worker's default of 30s.
      retry_delay_s: 1
    }

    assert :ok = Gateway.run(gw, [worker, worker, worker])

    s = Agent.get(seen, & &1)
    assert Enum.sort(s.values) == Enum.to_list(0..(n - 1))
    assert s.overlap == 0

    assert rest(ctx.base, :get, "/api/v0/tasks?queue=#{URI.encode_www_form(queue)}")["tasks"] in [
             nil,
             []
           ]

    docs = rest(ctx.base, :get, "/api/v0/docs?query.namespace=#{URI.encode_www_form(ns)}")["docs"]
    assert Enum.map(docs, & &1["content"]) == [n]
  end

  test "retries a task its work raises RetryError for, counting the attempt", ctx do
    queue = "ex-#{System.unique_integer([:positive])}/retry"

    rest(ctx.base, :post, "/api/v0/modify", %{
      claimantId: "seed",
      inserts: [%{queue: queue, value: "x"}]
    })

    gw = Gateway.new(entroq: ctx.entroq, eqlink: ctx.eqlink, log: fn _ -> nil end)
    {:ok, attempts} = Agent.start_link(fn -> [] end)

    worker = %Worker{
      queues: [queue],
      work: fn w, wctx ->
        Agent.update(attempts, &(&1 ++ [w.task["attempt"] || 0]))

        if Agent.get(attempts, &length/1) == 1 do
          raise EntroQ.RetryError, message: "not yet"
        end

        Context.stop(wctx)
        M.of([M.deleting(w.task)])
      end,
      retry_delay_s: 1
    }

    assert :ok = Gateway.run(gw, [worker])
    assert Agent.get(attempts, & &1) == [0, 1]

    assert rest(ctx.base, :get, "/api/v0/tasks?queue=#{URI.encode_www_form(queue)}")["tasks"] in [
             nil,
             []
           ]
  end

  defp free_port do
    {:ok, sock} = :gen_tcp.listen(0, ip: {127, 0, 0, 1})
    {:ok, port} = :inet.port(sock)
    :gen_tcp.close(sock)
    port
  end

  defp wait_until_serving(base, tries) do
    case :httpc.request(String.to_charlist(base <> "/api/v0/time")) do
      {:ok, {{_, 200, _}, _, _}} ->
        :ok

      _ when tries > 0 ->
        Process.sleep(100)
        wait_until_serving(base, tries - 1)
    end
  end

  defp rest(base, method, route, body \\ nil) do
    url = String.to_charlist(base <> route)
    request = if body, do: {url, [], ~c"application/json", JSON.encode!(body)}, else: {url, []}
    {:ok, {{_, status, _}, _, resp}} = :httpc.request(method, request, [], body_format: :binary)
    assert status == 200, resp
    if resp == "", do: %{}, else: JSON.decode!(resp)
  end
end
