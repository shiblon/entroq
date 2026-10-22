defmodule EntroQ.GatewayTest do
  use ExUnit.Case, async: true

  alias EntroQ.{Context, Gateway, GatewayError, Session, Worker}
  alias EntroQ.Modification, as: M

  @task %{
    "queue" => "in",
    "id" => "t1",
    "version" => 3,
    "claimantId" => "c",
    "value" => %{"n" => 1},
    "claims" => 1,
    "attempt" => 0
  }

  @doc_ %{
    "namespace" => "ns",
    "id" => "d1",
    "version" => 2,
    "key" => "k",
    "secondaryKey" => "s",
    "content" => %{"a" => 1}
  }

  # A scripted gateway: answer.(msg, sent) returns the reply to each message.
  # It sees each message as the wire would carry it, and sent/1 returns every
  # message so far, in order.
  defp fake(answer) do
    {:ok, log} = Agent.start_link(fn -> [] end)

    exchange = fn msg ->
      msg = msg |> JSON.encode!() |> JSON.decode!()
      sent = Agent.get_and_update(log, fn s -> {s ++ [msg], s ++ [msg]} end)

      case answer.(msg, sent) do
        %{"type" => "error"} = reply -> raise Gateway.failure(reply)
        reply -> reply
      end
    end

    {exchange, fn -> Agent.get(log, & &1) end}
  end

  # A gateway that hands out one task, then answers the next request with quit.
  defp one_task(opts \\ []) do
    docs = opts[:docs]

    fake(fn msg, _sent ->
      case msg["type"] do
        "config" ->
          %{"type" => "ready", "session" => "s1", "silence_timeout_s" => opts[:silence_s] || 0}

        "ready" when docs ->
          %{"type" => "take", "task" => @task}

        "ready" ->
          %{"type" => "work", "task" => @task, "doc_sets" => []}

        "take" ->
          %{
            "type" => "work",
            "task" => @task,
            "doc_sets" => [%{"set" => %{"namespace" => "ns", "key" => "k"}, "docs" => [@doc_]}]
          }

        "modify" ->
          if opts[:success] && !msg["outcome"],
            do: %{"type" => "success"},
            else: %{"type" => "quit"}

        "working" ->
          %{"type" => "ok"}

        _ ->
          %{"type" => "quit"}
      end
    end)
  end

  defp run_session(worker, exchange),
    do: Session.run(worker, exchange, [:atomics.new(1, []), :atomics.new(1, [])])

  describe "Session" do
    test "registers, works a task, and commits what work returns" do
      {xchg, sent} = one_task()

      w = %Worker{
        queues: ["in"],
        work: fn w, _ctx -> M.of([M.deleting(w.task)]) end,
        max_attempts: 3
      }

      :ok = run_session(w, xchg)

      [config, ready, modify] = sent.()
      assert config["config"]["queues"] == ["in"]
      assert config["config"]["max_attempts"] == 3
      assert config["config"]["want_docs"] == false
      assert ready["session"] == "s1"

      assert modify["modification"] == %{
               "deletes" => [%{"queue" => "in", "id" => "t1", "version" => 3}]
             }
    end

    test "names doc sets in take and hands work their docs" do
      {xchg, sent} = one_task(docs: true, success: true)
      me = self()

      w = %Worker{
        queues: ["in"],
        take: fn _w, _ctx -> [EntroQ.claim_set("ns", "k")] end,
        work: fn w, _ctx ->
          send(me, {:worked, w})
          [doc] = w.docs
          M.of([M.deleting(w.task), M.changing(doc, content: %{a: 2})])
        end,
        on_success: fn _w, _ctx -> send(me, :succeeded) end
      }

      :ok = run_session(w, xchg)
      assert_received {:worked, %{docs: [@doc_], sets: [%{"key" => "k"}], task: @task}}
      assert_received :succeeded

      take = Enum.find(sent.(), &(&1["type"] == "take"))

      assert take["doc_set_claims"] == [
               %{"set" => %{"namespace" => "ns", "key" => "k"}, "omit_members" => false}
             ]

      modify = Enum.find(sent.(), &(&1["type"] == "modify"))
      # A doc change keeps the keys it was handed, the secondary one included.
      assert [%{"new_data" => %{"secondaryKey" => "s", "content" => %{"a" => 2}}}] =
               modify["modification"]["doc_changes"]
    end

    test "turns what work raises into the task's fate" do
      for {raised, outcome, queue} <- [
            {%EntroQ.RetryError{message: "later", or_move_to: "dead"}, "retry", "dead"},
            {%EntroQ.MoveError{message: "now", queue: "elsewhere"}, "move", "elsewhere"},
            {%EntroQ.FatalWorker{message: "doomed"}, "fatal", nil},
            {%RuntimeError{message: "boom"}, "error", nil}
          ] do
        {xchg, sent} = one_task()
        :ok = run_session(%Worker{queues: ["in"], work: fn _w, _ctx -> raise raised end}, xchg)
        modify = List.last(sent.())
        assert modify["outcome"] == outcome
        assert modify["error"]["queue"] == queue
        refute Map.has_key?(modify, "modification")
      end
    end

    test "answers an idle ready with ready until a task comes" do
      {xchg, sent} =
        fake(fn msg, sent ->
          case {msg["type"], length(sent)} do
            {"config", _} -> %{"type" => "ready", "session" => "s1"}
            {"ready", n} when n < 4 -> %{"type" => "ready"}
            {"ready", _} -> %{"type" => "work", "task" => @task}
            _ -> %{"type" => "quit"}
          end
        end)

      :ok = run_session(%Worker{queues: ["in"], work: fn _w, _ctx -> nil end}, xchg)
      assert Enum.map(sent.(), & &1["type"]) == ["config", "ready", "ready", "ready", "modify"]
    end

    test "says quit on every message once the worker stops itself" do
      {xchg, sent} = one_task(success: true)

      w = %Worker{
        queues: ["in"],
        work: fn w, ctx ->
          Context.stop(ctx)
          assert Context.stopping?(ctx)
          M.of([M.deleting(w.task)])
        end,
        on_success: fn _w, _ctx -> nil end
      }

      :ok = run_session(w, xchg)
      {before, after_} = Enum.split(sent.(), 2)
      assert Enum.all?(before, &(!&1["quit"]))
      assert Enum.all?(after_, &(&1["quit"] == true))
    end

    test "keeps long work alive, and sends nothing after the answer" do
      {xchg, sent} = one_task(silence_s: 0.06)
      w = %Worker{queues: ["in"], work: fn _w, _ctx -> Process.sleep(70) end}
      :ok = run_session(w, xchg)
      types = Enum.map(sent.(), & &1["type"])
      assert Enum.count(types, &(&1 == "working")) >= 2
      assert List.last(types) == "modify"
    end

    test "serves reads mid-turn" do
      {xchg, _sent} =
        fake(fn msg, _sent ->
          case msg["type"] do
            "config" -> %{"type" => "ready", "session" => "s1"}
            "ready" -> %{"type" => "work", "task" => @task}
            "time" -> %{"type" => "time", "time_ms" => "1234"}
            _ -> %{"type" => "quit"}
          end
        end)

      me = self()

      :ok =
        run_session(
          %Worker{queues: ["in"], work: fn _w, ctx -> send(me, {:time, Context.time(ctx)}) end},
          xchg
        )

      assert_received {:time, 1234}
    end

    test "lets a read's error end the session, whatever the function wanted" do
      {xchg, sent} =
        fake(fn msg, _sent ->
          case msg["type"] do
            "config" -> %{"type" => "ready", "session" => "s1"}
            "ready" -> %{"type" => "work", "task" => @task}
            "time" -> %{"type" => "error", "class" => "caller", "message" => "no"}
          end
        end)

      w = %Worker{queues: ["in"], work: fn _w, ctx -> Context.time(ctx) end}
      assert_raise GatewayError, "[caller] no", fn -> run_session(w, xchg) end
      refute Enum.any?(sent.(), &(&1["type"] == "modify"))
    end
  end

  describe "Modification" do
    test "passes fields through, by_ms included" do
      m =
        M.of([
          M.inserting(queue: "next", value: 1, by_ms: 1500),
          M.changing(@task, queue: "later", by_ms: 20)
        ])

      assert m.request["inserts"] == [%{"queue" => "next", "value" => 1, "by_ms" => 1500}]

      assert m.request["changes"] == [
               %{
                 "old_id" => %{"queue" => "in", "id" => "t1", "version" => 3},
                 "new_data" => %{"queue" => "later", "value" => %{"n" => 1}, "by_ms" => 20}
               }
             ]
    end

    test "names docs and tasks by id and version" do
      m =
        M.of([
          M.depending(@doc_),
          M.deleting(@doc_),
          M.inserting(namespace: "ns", key: "k2", content: 3)
        ])

      assert m.request == %{
               "doc_depends" => [%{"namespace" => "ns", "id" => "d1", "version" => 2}],
               "doc_deletes" => [%{"namespace" => "ns", "id" => "d1", "version" => 2}],
               "doc_inserts" => [%{"namespace" => "ns", "key" => "k2", "content" => 3}]
             }
    end
  end

  describe "Gateway" do
    defp counter, do: elem(Agent.start_link(fn -> 0 end), 1)

    test "starts a new session at once when one is gone" do
      sessions = counter()

      gw =
        Gateway.new(
          log: fn _ -> nil end,
          url: "unused"
        )

      gw = %{
        gw
        | connect: fn ->
            case Agent.get_and_update(sessions, &{&1 + 1, &1 + 1}) do
              1 ->
                elem(
                  fake(fn _, _ ->
                    %{"type" => "error", "class" => "gone", "message" => "gone"}
                  end),
                  0
                )

              _ ->
                elem(one_task(), 0)
            end
          end
      }

      me = self()

      w = %Worker{
        queues: ["in"],
        work: fn w, ctx ->
          send(me, :worked)
          Context.stop(ctx)
          M.of([M.deleting(w.task)])
        end
      }

      assert :ok = Gateway.run(gw, [w])
      assert Agent.get(sessions, & &1) == 2
      assert_received :worked
    end

    test "returns a failure no new session can fix, and stops every worker" do
      gw = %{
        Gateway.new(log: fn _ -> nil end, url: "unused")
        | connect: fn ->
            elem(
              fake(fn _, _ ->
                %{"type" => "error", "class" => "caller", "message" => "bad config"}
              end),
              0
            )
          end
      }

      w = %Worker{queues: ["in"], work: fn _w, _ctx -> nil end}
      assert {:error, %GatewayError{message: "[caller] bad config"}} = Gateway.run(gw, [w, w])
    end

    test "drains on stop: the task in hand finishes, and the session says quit" do
      {xchg, sent} = one_task()
      gw = %{Gateway.new(log: fn _ -> nil end, url: "unused") | connect: fn -> xchg end}
      me = self()

      w = %Worker{
        queues: ["in"],
        work: fn w, _ctx ->
          send(me, :holding)
          Process.sleep(20)
          M.of([M.deleting(w.task)])
        end
      }

      running = Task.async(fn -> Gateway.run(gw, [w]) end)
      assert_receive :holding
      Gateway.stop(gw)
      assert :ok = Task.await(running)
      modify = Enum.find(sent.(), &(&1["type"] == "modify"))

      assert modify["modification"] == %{
               "deletes" => [%{"queue" => "in", "id" => "t1", "version" => 3}]
             }

      assert modify["quit"] == true
    end
  end
end
