defmodule EntroQ.DependencyError do
  @moduledoc """
  Optimistic-concurrency failure returned by EntroQ `Modify`/`ClaimDocs`.
  """

  alias EntroQ.{DocID, TaskID}

  defexception message: "",
               inserts: [],
               changes: [],
               deletes: [],
               depends: [],
               claims: [],
               doc_inserts: [],
               doc_changes: [],
               doc_deletes: [],
               doc_depends: [],
               doc_claims: [],
               details: []

  @dep_types MapSet.new(["INSERT", "CHANGE", "DELETE", "DEPEND", "CLAIM", "DETAIL"])

  @type t :: %__MODULE__{
          message: String.t(),
          inserts: [TaskID.t()],
          changes: [TaskID.t()],
          deletes: [TaskID.t()],
          depends: [TaskID.t()],
          claims: [TaskID.t()],
          doc_inserts: [DocID.t()],
          doc_changes: [DocID.t()],
          doc_deletes: [DocID.t()],
          doc_depends: [DocID.t()],
          doc_claims: [DocID.t()],
          details: [map()]
        }

  @doc false
  def dependency_json?(%{"details" => details}) when is_list(details) do
    Enum.any?(details, fn detail -> MapSet.member?(@dep_types, detail["type"]) end)
  end

  def dependency_json?(_), do: false

  @doc false
  def from_json(%{"details" => details} = data) do
    Enum.reduce(
      details,
      %__MODULE__{message: data["message"] || "", details: details},
      &put_detail/2
    )
  end

  def from_json(data) do
    %__MODULE__{message: inspect(data), details: []}
  end

  defp put_detail(%{"type" => "DETAIL", "msg" => message}, error) when is_binary(message) do
    %{error | message: message}
  end

  defp put_detail(%{"type" => type, "id" => id}, error) when is_map(id) do
    put_task_id(error, type, TaskID.from_json(id))
  end

  defp put_detail(%{"type" => type, "docId" => id}, error) when is_map(id) do
    put_doc_id(error, type, DocID.from_json(id))
  end

  defp put_detail(_detail, error), do: error

  defp put_task_id(error, "INSERT", id), do: %{error | inserts: error.inserts ++ [id]}
  defp put_task_id(error, "CHANGE", id), do: %{error | changes: error.changes ++ [id]}
  defp put_task_id(error, "DELETE", id), do: %{error | deletes: error.deletes ++ [id]}
  defp put_task_id(error, "DEPEND", id), do: %{error | depends: error.depends ++ [id]}
  defp put_task_id(error, "CLAIM", id), do: %{error | claims: error.claims ++ [id]}
  defp put_task_id(error, _type, _id), do: error

  defp put_doc_id(error, "INSERT", id), do: %{error | doc_inserts: error.doc_inserts ++ [id]}
  defp put_doc_id(error, "CHANGE", id), do: %{error | doc_changes: error.doc_changes ++ [id]}
  defp put_doc_id(error, "DELETE", id), do: %{error | doc_deletes: error.doc_deletes ++ [id]}
  defp put_doc_id(error, "DEPEND", id), do: %{error | doc_depends: error.doc_depends ++ [id]}
  defp put_doc_id(error, "CLAIM", id), do: %{error | doc_claims: error.doc_claims ++ [id]}
  defp put_doc_id(error, _type, _id), do: error
end
