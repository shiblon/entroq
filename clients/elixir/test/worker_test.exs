defmodule EntroQ.WorkerTest do
  use ExUnit.Case

  alias EntroQ.{Doc, DocClaim, Modification, ModifyResult, Task}

  defmodule FakeClient do
    defstruct agent: nil

    def start_link(opts \\ []) do
      {:ok, agent} =
        Agent.start_link(fn ->
          %{
            tasks: Keyword.get(opts, :tasks, []),
            docs: Keyword.get(opts, :docs, []),
            modifications: [],
            claim_order: []
          }
        end)

      %__MODULE__{agent: agent}
    end

    def claim(client, _queues, _opts) do
      Agent.get_and_update(client.agent, fn
        %{tasks: [task | rest]} = state -> {{:ok, task}, %{state | tasks: rest}}
        state -> {{:error, :empty}, state}
      end)
    end

    def claim_docs(client, %DocClaim{} = claim) do
      Agent.get_and_update(client.agent, fn state ->
        docs =
          Enum.filter(state.docs, fn doc ->
            doc.namespace == claim.namespace and doc.key == claim.key
          end)

        state = %{
          state
          | claim_order: state.claim_order ++ [{claim.namespace, claim.key, claim.duration_ms}]
        }

        {{:ok, docs}, state}
      end)
    end

    def modify(client, %Modification{} = modification, _opts) do
      Agent.get_and_update(client.agent, fn state ->
        changed_tasks =
          Enum.map(modification.changes, fn change ->
            %Task{
              id: change.old_id.id,
              version: change.old_id.version + 1,
              queue: change.new_data.queue,
              at_ms: change.new_data.at_ms,
              value: change.new_data.value,
              attempt: change.new_data.attempt,
              err: change.new_data.err
            }
          end)

        changed_docs =
          Enum.map(modification.doc_changes, fn change ->
            %Doc{
              namespace: change.old_id.namespace,
              id: change.old_id.id,
              version: change.old_id.version + 1,
              key: change.new_data.key,
              secondary_key: change.new_data.secondary_key,
              content: change.new_data.content,
              at_ms: change.new_data.at_ms
            }
          end)

        result = %ModifyResult{tasks_changed: changed_tasks, docs_changed: changed_docs}
        state = %{state | modifications: state.modifications ++ [modification]}
        {{:ok, result}, state}
      end)
    end

    def modifications(client), do: Agent.get(client.agent, & &1.modifications)
    def claim_order(client), do: Agent.get(client.agent, & &1.claim_order)
  end

  defmodule ModuleWorker do
    use EntroQ.Worker

    @impl true
    def perform(task, _docs) do
      {:modify, Modification.delete(task)}
    end
  end

  defp task(attrs \\ []) do
    struct(
      %Task{
        id: "t1",
        version: 1,
        queue: "q",
        at_ms: 0,
        value: %{"work" => true},
        attempt: 0
      },
      attrs
    )
  end

  defp doc(attrs \\ []) do
    struct(
      %Doc{
        namespace: "ns",
        id: "d1",
        version: 1,
        key: "k",
        content: %{"state" => true}
      },
      attrs
    )
  end

  test "module worker applies returned modification" do
    client = FakeClient.start_link(tasks: [task()])

    assert {:ok, %ModifyResult{}} =
             EntroQ.Worker.run_once(client, ["q"], ModuleWorker, lease_ms: 10_000)

    assert [final] = FakeClient.modifications(client)
    assert [%EntroQ.TaskID{id: "t1", version: 1, queue: "q"}] = final.deletes
  end

  test "function worker sorts doc claims before acquiring docs" do
    client = FakeClient.start_link(tasks: [task()], docs: [])

    handler = [
      take_docs: fn _task ->
        [
          DocClaim.new("b", "2"),
          DocClaim.new("a", "9"),
          DocClaim.new("a", "1")
        ]
      end,
      perform: fn _task, _docs -> :ok end
    ]

    assert :ok = EntroQ.Worker.run_once(client, ["q"], handler, lease_ms: 10_000)

    assert FakeClient.claim_order(client) == [
             {"a", "1", 10_000},
             {"a", "9", 10_000},
             {"b", "2", 10_000}
           ]
  end

  test "run continues after successful modify and stops on :stop" do
    client = FakeClient.start_link(tasks: [task(id: "t1"), task(id: "t2")])

    handler = [
      perform: fn
        %Task{id: "t1"} = task, _docs -> {:modify, Modification.delete(task)}
        %Task{id: "t2"}, _docs -> :stop
      end
    ]

    assert :ok = EntroQ.Worker.run(client, ["q"], handler, lease_ms: 10_000)

    assert [final] = FakeClient.modifications(client)
    assert [%EntroQ.TaskID{id: "t1"}] = final.deletes
  end

  test "final modification uses versions captured after renewal stops" do
    client = FakeClient.start_link(tasks: [task()])

    handler = [
      perform: fn task, _docs ->
        Process.sleep(30)
        {:modify, Modification.delete(task)}
      end
    ]

    assert {:ok, %ModifyResult{}} = EntroQ.Worker.run_once(client, ["q"], handler, lease_ms: 10)

    modifications = FakeClient.modifications(client)
    assert length(modifications) >= 2
    final = List.last(modifications)
    renewal_count = length(modifications) - 1
    assert [%EntroQ.TaskID{id: "t1", version: version}] = final.deletes
    assert version == 1 + renewal_count
  end

  test "finish receives final renewed versions" do
    client = FakeClient.start_link(tasks: [task()], docs: [doc()])

    handler = [
      take_docs: fn _task -> [DocClaim.new("ns", "k")] end,
      perform: fn _task, _docs ->
        Process.sleep(30)
        {:finish, :payload}
      end,
      finish: fn final_task, final_docs, :payload ->
        assert final_task.version > 1
        assert [%Doc{version: doc_version}] = final_docs
        assert doc_version > 1
        {:modify, Modification.delete(final_task)}
      end
    ]

    assert {:ok, %ModifyResult{}} = EntroQ.Worker.run_once(client, ["q"], handler, lease_ms: 10)

    final = client |> FakeClient.modifications() |> List.last()
    assert [%EntroQ.TaskID{version: version}] = final.deletes
    assert version > 1
  end

  test "retry releases docs and increments attempt" do
    client = FakeClient.start_link(tasks: [task()], docs: [doc()])

    handler = [
      take_docs: fn _task -> [DocClaim.new("ns", "k")] end,
      perform: fn _task, _docs -> {:retry, "later", delay_ms: 1_000} end
    ]

    assert {:ok, %ModifyResult{}} =
             EntroQ.Worker.run_once(client, ["q"], handler, lease_ms: 10_000)

    final = client |> FakeClient.modifications() |> List.last()

    assert [%EntroQ.TaskChange{new_data: %EntroQ.TaskData{attempt: 1, err: "later"}}] =
             final.changes

    assert [%EntroQ.DocChange{new_data: %EntroQ.DocData{at_ms: 0}}] = final.doc_changes
  end
end
