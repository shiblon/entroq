defmodule EntroQ.ModificationTest do
  use ExUnit.Case, async: true

  alias EntroQ.{Doc, Modification, Task}

  test "fix_versions updates renewed task and doc versions" do
    task = %Task{id: "t1", version: 9, queue: "q"}
    doc = %Doc{namespace: "ns", id: "d1", version: 7, key: "k"}

    stale_task = %{task | version: 1}
    stale_doc = %{doc | version: 2}

    mod =
      Modification.new()
      |> Modification.delete(stale_task)
      |> Modification.change(stale_doc, content: %{"n" => 1})
      |> Modification.depend(stale_doc)

    fixed = Modification.fix_versions(mod, task, [doc])

    assert [%EntroQ.TaskID{version: 9}] = fixed.deletes
    assert [%EntroQ.DocChange{old_id: %EntroQ.DocID{version: 7}}] = fixed.doc_changes
    assert [%EntroQ.DocID{version: 7}] = fixed.doc_depends
  end

  test "encodes task and doc operations for Modify" do
    task = %Task{id: "t1", version: 1, queue: "q", value: %{"x" => 1}}
    doc = %Doc{namespace: "ns", id: "d1", version: 1, key: "k", content: %{"c" => 1}}

    json =
      Modification.new()
      |> Modification.change(task, at_ms: 123)
      |> Modification.delete(doc)
      |> Modification.to_json("worker")

    assert json["claimantId"] == "worker"
    assert [%{"oldId" => %{"id" => "t1", "version" => 1, "queue" => "q"}}] = json["changes"]
    assert [%{"namespace" => "ns", "id" => "d1", "version" => 1}] = json["docDeletes"]
  end
end
