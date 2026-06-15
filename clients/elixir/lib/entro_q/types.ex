defmodule EntroQ.TaskID do
  @moduledoc """
  Versioned task identity.
  """

  defstruct id: "", version: 0, queue: ""

  @type t :: %__MODULE__{id: String.t(), version: integer(), queue: String.t()}

  def from_json(data) do
    %__MODULE__{
      id: data["id"] || "",
      version: int(data["version"]),
      queue: data["queue"] || ""
    }
  end

  def to_json(%__MODULE__{} = id) do
    %{"id" => id.id, "version" => id.version, "queue" => id.queue}
  end

  defp int(nil), do: 0
  defp int(value) when is_integer(value), do: value
  defp int(value) when is_binary(value), do: String.to_integer(value)
end

defmodule EntroQ.TaskData do
  @moduledoc """
  Data for inserting or changing a task.
  """

  defstruct queue: "", at_ms: 0, value: nil, id: nil, attempt: 0, err: ""

  @type t :: %__MODULE__{
          queue: String.t(),
          at_ms: integer(),
          value: term(),
          id: String.t() | nil,
          attempt: integer(),
          err: String.t()
        }

  def to_json(%__MODULE__{} = data, opts \\ []) do
    include_id? = Keyword.get(opts, :include_id, true)

    %{
      "queue" => data.queue,
      "atMs" => Integer.to_string(data.at_ms || 0),
      "value" => data.value,
      "attempt" => data.attempt || 0,
      "err" => data.err || ""
    }
    |> maybe_put("id", include_id? && data.id)
  end

  defp maybe_put(map, _key, nil), do: map
  defp maybe_put(map, _key, false), do: map
  defp maybe_put(map, key, value), do: Map.put(map, key, value)
end

defmodule EntroQ.TaskChange do
  @moduledoc """
  Versioned task change.
  """

  alias EntroQ.{TaskData, TaskID}

  defstruct old_id: %TaskID{}, new_data: %TaskData{}

  @type t :: %__MODULE__{old_id: TaskID.t(), new_data: TaskData.t()}

  def to_json(%__MODULE__{} = change) do
    %{
      "oldId" => TaskID.to_json(change.old_id),
      "newData" => TaskData.to_json(change.new_data, include_id: false)
    }
  end
end

defmodule EntroQ.Task do
  @moduledoc """
  Complete task returned by EntroQ.
  """

  alias EntroQ.{TaskChange, TaskData, TaskID}

  defstruct queue: "",
            id: "",
            version: 0,
            at_ms: 0,
            claimant_id: "",
            value: nil,
            created_ms: 0,
            modified_ms: 0,
            claims: 0,
            attempt: 0,
            err: ""

  @type t :: %__MODULE__{
          queue: String.t(),
          id: String.t(),
          version: integer(),
          at_ms: integer(),
          claimant_id: String.t(),
          value: term(),
          created_ms: integer(),
          modified_ms: integer(),
          claims: integer(),
          attempt: integer(),
          err: String.t()
        }

  def from_json(data) do
    %__MODULE__{
      queue: data["queue"] || "",
      id: data["id"] || "",
      version: int(data["version"]),
      at_ms: int(data["atMs"]),
      claimant_id: data["claimantId"] || "",
      value: data["value"],
      created_ms: int(data["createdMs"]),
      modified_ms: int(data["modifiedMs"]),
      claims: int(data["claims"]),
      attempt: int(data["attempt"]),
      err: data["err"] || ""
    }
  end

  def as_id(%__MODULE__{} = task) do
    %TaskID{id: task.id, version: task.version, queue: task.queue}
  end

  def as_change(%__MODULE__{} = task, attrs \\ []) do
    %TaskChange{
      old_id: as_id(task),
      new_data: %TaskData{
        queue: Keyword.get(attrs, :queue, task.queue),
        at_ms: Keyword.get(attrs, :at_ms, task.at_ms),
        value: Keyword.get(attrs, :value, task.value),
        attempt: Keyword.get(attrs, :attempt, task.attempt),
        err: Keyword.get(attrs, :err, task.err)
      }
    }
  end

  defp int(nil), do: 0
  defp int(value) when is_integer(value), do: value
  defp int(value) when is_binary(value), do: String.to_integer(value)
end

defmodule EntroQ.QueueStats do
  @moduledoc """
  Queue statistics.
  """

  defstruct name: "", num_tasks: 0, num_claimed: 0, num_available: 0, max_claims: 0, num_future: 0

  @type t :: %__MODULE__{
          name: String.t(),
          num_tasks: integer(),
          num_claimed: integer(),
          num_available: integer(),
          max_claims: integer(),
          num_future: integer()
        }

  def from_json(data) do
    %__MODULE__{
      name: data["name"] || "",
      num_tasks: int(data["numTasks"]),
      num_claimed: int(data["numClaimed"]),
      num_available: int(data["numAvailable"]),
      max_claims: int(data["maxClaims"]),
      num_future: int(data["numFuture"])
    }
  end

  defp int(nil), do: 0
  defp int(value) when is_integer(value), do: value
  defp int(value) when is_binary(value), do: String.to_integer(value)
end

defmodule EntroQ.DocID do
  @moduledoc """
  Versioned doc identity.
  """

  defstruct namespace: "", id: "", version: 0

  @type t :: %__MODULE__{namespace: String.t(), id: String.t(), version: integer()}

  def from_json(data) do
    %__MODULE__{
      namespace: data["namespace"] || "",
      id: data["id"] || "",
      version: int(data["version"])
    }
  end

  def to_json(%__MODULE__{} = id) do
    %{"namespace" => id.namespace, "id" => id.id, "version" => id.version}
  end

  defp int(nil), do: 0
  defp int(value) when is_integer(value), do: value
  defp int(value) when is_binary(value), do: String.to_integer(value)
end

defmodule EntroQ.DocData do
  @moduledoc """
  Data for inserting or changing a doc.
  """

  defstruct namespace: "",
            id: nil,
            key: "",
            secondary_key: "",
            content: nil,
            at_ms: 0,
            created_ms: 0,
            modified_ms: 0

  @type t :: %__MODULE__{
          namespace: String.t(),
          id: String.t() | nil,
          key: String.t(),
          secondary_key: String.t(),
          content: term(),
          at_ms: integer(),
          created_ms: integer(),
          modified_ms: integer()
        }

  def to_json(%__MODULE__{} = data, opts \\ []) do
    mode = Keyword.get(opts, :mode, :insert)

    %{
      "namespace" => data.namespace,
      "key" => data.key,
      "secondaryKey" => data.secondary_key || "",
      "content" => data.content
    }
    |> maybe_put("id", data.id)
    |> maybe_put("atMs", mode == :change && Integer.to_string(data.at_ms || 0))
    |> maybe_put("createdMs", data.created_ms != 0 && Integer.to_string(data.created_ms))
    |> maybe_put("modifiedMs", data.modified_ms != 0 && Integer.to_string(data.modified_ms))
  end

  defp maybe_put(map, _key, nil), do: map
  defp maybe_put(map, _key, false), do: map
  defp maybe_put(map, key, value), do: Map.put(map, key, value)
end

defmodule EntroQ.DocChange do
  @moduledoc """
  Versioned doc change.
  """

  alias EntroQ.{DocData, DocID}

  defstruct old_id: %DocID{}, new_data: %DocData{}

  @type t :: %__MODULE__{old_id: DocID.t(), new_data: DocData.t()}

  def to_json(%__MODULE__{} = change) do
    %{
      "oldId" => DocID.to_json(change.old_id),
      "newData" => DocData.to_json(change.new_data, mode: :change)
    }
  end
end

defmodule EntroQ.Doc do
  @moduledoc """
  Complete doc returned by EntroQ.
  """

  alias EntroQ.{DocChange, DocData, DocID}

  defstruct namespace: "",
            id: "",
            version: 0,
            claimant: "",
            at_ms: 0,
            key: "",
            secondary_key: "",
            content: nil,
            created_ms: 0,
            modified_ms: 0

  @type t :: %__MODULE__{
          namespace: String.t(),
          id: String.t(),
          version: integer(),
          claimant: String.t(),
          at_ms: integer(),
          key: String.t(),
          secondary_key: String.t(),
          content: term(),
          created_ms: integer(),
          modified_ms: integer()
        }

  def from_json(data) do
    %__MODULE__{
      namespace: data["namespace"] || "",
      id: data["id"] || "",
      version: int(data["version"]),
      claimant: data["claimant"] || "",
      at_ms: int(data["atMs"]),
      key: data["key"] || "",
      secondary_key: data["secondaryKey"] || "",
      content: data["content"],
      created_ms: int(data["createdMs"]),
      modified_ms: int(data["modifiedMs"])
    }
  end

  def as_id(%__MODULE__{} = doc) do
    %DocID{namespace: doc.namespace, id: doc.id, version: doc.version}
  end

  def as_change(%__MODULE__{} = doc, attrs \\ []) do
    %DocChange{
      old_id: as_id(doc),
      new_data: %DocData{
        namespace: Keyword.get(attrs, :namespace, doc.namespace),
        id: doc.id,
        key: Keyword.get(attrs, :key, doc.key),
        secondary_key: Keyword.get(attrs, :secondary_key, doc.secondary_key),
        content: Keyword.get(attrs, :content, doc.content),
        at_ms: Keyword.get(attrs, :at_ms, 0)
      }
    }
  end

  defp int(nil), do: 0
  defp int(value) when is_integer(value), do: value
  defp int(value) when is_binary(value), do: String.to_integer(value)
end

defmodule EntroQ.DocClaim do
  @moduledoc """
  Describes docs to claim by namespace and primary key.
  """

  defstruct namespace: "", key: "", duration_ms: nil, claimant: nil

  @type t :: %__MODULE__{
          namespace: String.t(),
          key: String.t(),
          duration_ms: integer() | nil,
          claimant: String.t() | nil
        }

  def new(%__MODULE__{} = claim), do: claim

  def new(opts) when is_list(opts) do
    %__MODULE__{
      namespace: Keyword.fetch!(opts, :namespace),
      key: Keyword.fetch!(opts, :key),
      duration_ms: Keyword.get(opts, :duration_ms),
      claimant: Keyword.get(opts, :claimant)
    }
  end

  def new(namespace, key, opts \\ []) do
    new(Keyword.merge(opts, namespace: namespace, key: key))
  end

  def to_json(%__MODULE__{} = claim) do
    %{
      "namespace" => claim.namespace,
      "claimant" => claim.claimant || "",
      "key" => claim.key,
      "durationMs" => Integer.to_string(claim.duration_ms || 30_000)
    }
  end
end

defmodule EntroQ.DocQuery do
  @moduledoc false

  defstruct namespace: "",
            key_start: "",
            key_end: "",
            key_exact: "",
            limit: 0,
            omit_values: false,
            ids: []

  def to_query_params(%__MODULE__{} = query) do
    []
    |> put_if("query.namespace", query.namespace)
    |> put_if("query.keyStart", query.key_start)
    |> put_if("query.keyEnd", query.key_end)
    |> put_if("query.keyExact", query.key_exact)
    |> put_if("query.limit", query.limit)
    |> put_bool("query.omitValues", query.omit_values)
    |> put_repeated("query.ids", query.ids)
  end

  defp put_if(query, _key, nil), do: query
  defp put_if(query, _key, ""), do: query
  defp put_if(query, _key, 0), do: query
  defp put_if(query, key, value), do: [{key, value} | query]

  defp put_bool(query, _key, false), do: query
  defp put_bool(query, key, true), do: [{key, "true"} | query]

  defp put_repeated(query, _key, nil), do: query
  defp put_repeated(query, _key, []), do: query
  defp put_repeated(query, key, values), do: Enum.reduce(values, query, &put_if(&2, key, &1))
end

defmodule EntroQ.NamespaceStat do
  @moduledoc """
  Doc namespace statistics.
  """

  defstruct name: "", num_docs: 0, num_claimed: 0

  @type t :: %__MODULE__{name: String.t(), num_docs: integer(), num_claimed: integer()}

  def from_json(data) do
    %__MODULE__{
      name: data["name"] || "",
      num_docs: int(data["numDocs"]),
      num_claimed: int(data["numClaimed"])
    }
  end

  defp int(nil), do: 0
  defp int(value) when is_integer(value), do: value
  defp int(value) when is_binary(value), do: String.to_integer(value)
end

defmodule EntroQ.ClaimRequest do
  @moduledoc false

  defstruct claimant_id: "", queues: [], duration_ms: 30_000, poll_ms: 5_000

  def to_json(%__MODULE__{} = request) do
    %{
      "claimantId" => request.claimant_id,
      "queues" => request.queues,
      "durationMs" => Integer.to_string(request.duration_ms),
      "pollMs" => Integer.to_string(request.poll_ms)
    }
  end
end

defmodule EntroQ.ClaimDocsRequest do
  @moduledoc false

  alias EntroQ.DocClaim

  defstruct claim_query: %DocClaim{}

  def to_json(%__MODULE__{} = request) do
    %{"claimQuery" => DocClaim.to_json(request.claim_query)}
  end
end

defmodule EntroQ.ModifyResult do
  @moduledoc """
  Result returned from a successful `Modify` call.
  """

  alias EntroQ.{Doc, Task}

  defstruct tasks_inserted: [], tasks_changed: [], docs_inserted: [], docs_changed: []

  @type t :: %__MODULE__{
          tasks_inserted: [Task.t()],
          tasks_changed: [Task.t()],
          docs_inserted: [Doc.t()],
          docs_changed: [Doc.t()]
        }

  def from_json(data) do
    %__MODULE__{
      tasks_inserted: Enum.map(data["inserted"] || [], &Task.from_json/1),
      tasks_changed: Enum.map(data["changed"] || [], &Task.from_json/1),
      docs_inserted: Enum.map(data["insertedDocs"] || [], &Doc.from_json/1),
      docs_changed: Enum.map(data["changedDocs"] || [], &Doc.from_json/1)
    }
  end
end
