defmodule EntroQ.Modification do
  @moduledoc """
  Builder for EntroQ's atomic task/doc `Modify` request.

  User worker code builds modifications against the task/docs it was given.
  `EntroQ.Worker` rewrites versions for renewed tasks/docs before sending the
  final request, preserving EntroQ's atomicity without exposing renewal races.
  """

  alias EntroQ.{Doc, DocChange, DocData, DocID, Task, TaskChange, TaskData, TaskID}

  defstruct inserts: [],
            changes: [],
            deletes: [],
            depends: [],
            doc_inserts: [],
            doc_changes: [],
            doc_deletes: [],
            doc_depends: []

  @type t :: %__MODULE__{
          inserts: [TaskData.t()],
          changes: [TaskChange.t()],
          deletes: [TaskID.t()],
          depends: [TaskID.t()],
          doc_inserts: [DocData.t()],
          doc_changes: [DocChange.t()],
          doc_deletes: [DocID.t()],
          doc_depends: [DocID.t()]
        }

  @doc """
  Returns an empty modification.
  """
  @spec new() :: t()
  def new, do: %__MODULE__{}

  @doc """
  Adds an insert operation.
  """
  @spec insert(TaskData.t() | DocData.t()) :: t()
  def insert(item), do: insert(new(), item)

  @spec insert(t(), TaskData.t() | DocData.t()) :: t()
  def insert(%__MODULE__{} = mod, %TaskData{} = data), do: %{mod | inserts: mod.inserts ++ [data]}

  def insert(%__MODULE__{} = mod, %DocData{} = data),
    do: %{mod | doc_inserts: mod.doc_inserts ++ [data]}

  @doc """
  Adds a change operation.
  """
  @spec change(Task.t() | TaskChange.t() | Doc.t() | DocChange.t(), keyword()) :: t()
  def change(item, attrs \\ []), do: change(new(), item, attrs)

  @spec change(t(), Task.t() | TaskChange.t() | Doc.t() | DocChange.t(), keyword()) :: t()
  def change(%__MODULE__{} = mod, %Task{} = task, attrs) do
    change(mod, Task.as_change(task, attrs), [])
  end

  def change(%__MODULE__{} = mod, %TaskChange{} = change, _attrs) do
    %{mod | changes: mod.changes ++ [change]}
  end

  def change(%__MODULE__{} = mod, %Doc{} = doc, attrs) do
    change(mod, Doc.as_change(doc, attrs), [])
  end

  def change(%__MODULE__{} = mod, %DocChange{} = change, _attrs) do
    %{mod | doc_changes: mod.doc_changes ++ [change]}
  end

  @doc """
  Adds a delete operation.
  """
  @spec delete(Task.t() | TaskID.t() | Doc.t() | DocID.t()) :: t()
  def delete(item), do: delete(new(), item)

  @spec delete(t(), Task.t() | TaskID.t() | Doc.t() | DocID.t()) :: t()
  def delete(%__MODULE__{} = mod, %Task{} = task), do: delete(mod, Task.as_id(task))
  def delete(%__MODULE__{} = mod, %TaskID{} = id), do: %{mod | deletes: mod.deletes ++ [id]}
  def delete(%__MODULE__{} = mod, %Doc{} = doc), do: delete(mod, Doc.as_id(doc))

  def delete(%__MODULE__{} = mod, %DocID{} = id),
    do: %{mod | doc_deletes: mod.doc_deletes ++ [id]}

  @doc """
  Adds a dependency check.
  """
  @spec depend(Task.t() | TaskID.t() | Doc.t() | DocID.t()) :: t()
  def depend(item), do: depend(new(), item)

  @spec depend(t(), Task.t() | TaskID.t() | Doc.t() | DocID.t()) :: t()
  def depend(%__MODULE__{} = mod, %Task{} = task), do: depend(mod, Task.as_id(task))
  def depend(%__MODULE__{} = mod, %TaskID{} = id), do: %{mod | depends: mod.depends ++ [id]}
  def depend(%__MODULE__{} = mod, %Doc{} = doc), do: depend(mod, Doc.as_id(doc))

  def depend(%__MODULE__{} = mod, %DocID{} = id),
    do: %{mod | doc_depends: mod.doc_depends ++ [id]}

  @doc """
  Rewrites task/doc versions to the final versions captured after renewal stops.
  """
  @spec fix_versions(t(), Task.t(), [Doc.t()]) :: t()
  def fix_versions(%__MODULE__{} = mod, %Task{} = task, docs) do
    doc_versions = Map.new(docs, fn doc -> {{doc.namespace, doc.id}, doc.version} end)

    %{
      mod
      | changes: Enum.map(mod.changes, &fix_task_change(&1, task)),
        deletes: Enum.map(mod.deletes, &fix_task_id(&1, task)),
        depends: Enum.map(mod.depends, &fix_task_id(&1, task)),
        doc_changes: Enum.map(mod.doc_changes, &fix_doc_change(&1, doc_versions)),
        doc_deletes: Enum.map(mod.doc_deletes, &fix_doc_id(&1, doc_versions)),
        doc_depends: Enum.map(mod.doc_depends, &fix_doc_id(&1, doc_versions))
    }
  end

  @doc false
  def to_json(%__MODULE__{} = mod, claimant_id) do
    %{
      "claimantId" => claimant_id,
      "inserts" => Enum.map(mod.inserts, &TaskData.to_json/1),
      "changes" => Enum.map(mod.changes, &TaskChange.to_json/1),
      "deletes" => Enum.map(mod.deletes, &TaskID.to_json/1),
      "depends" => Enum.map(mod.depends, &TaskID.to_json/1),
      "docInserts" => Enum.map(mod.doc_inserts, &DocData.to_json/1),
      "docChanges" => Enum.map(mod.doc_changes, &DocChange.to_json/1),
      "docDeletes" => Enum.map(mod.doc_deletes, &DocID.to_json/1),
      "docDepends" => Enum.map(mod.doc_depends, &DocID.to_json/1)
    }
  end

  defp fix_task_change(%TaskChange{} = change, task) do
    %{change | old_id: fix_task_id(change.old_id, task)}
  end

  defp fix_task_id(%TaskID{id: id} = old_id, %Task{id: id, version: version}) do
    %{old_id | version: version}
  end

  defp fix_task_id(%TaskID{} = old_id, _task), do: old_id

  defp fix_doc_change(%DocChange{} = change, doc_versions) do
    %{change | old_id: fix_doc_id(change.old_id, doc_versions)}
  end

  defp fix_doc_id(%DocID{} = old_id, doc_versions) do
    version = Map.get(doc_versions, {old_id.namespace, old_id.id})

    if is_nil(version) do
      old_id
    else
      %{old_id | version: version}
    end
  end
end
