defmodule EntroQ.Client do
  @moduledoc """
  HTTP/JSON client for EntroQ's `/api/v0` RPC surface.

  Functions return `{:ok, value}` or `{:error, reason}`. Dependency conflicts
  are returned as `%EntroQ.DependencyError{}` so callers can distinguish stale
  versions from ordinary transport errors.
  """

  alias EntroQ.{
    ClaimDocsRequest,
    ClaimRequest,
    DependencyError,
    Doc,
    DocClaim,
    DocQuery,
    Modification,
    ModifyResult,
    NamespaceStat,
    QueueStats,
    Task
  }

  defstruct base_url: "",
            claimant_id: "",
            headers: [],
            transport: EntroQ.Client.HTTP,
            transport_opts: []

  @type t :: %__MODULE__{
          base_url: String.t(),
          claimant_id: String.t(),
          headers: [{String.t(), String.t()}],
          transport: module(),
          transport_opts: keyword()
        }

  defmodule Error do
    @moduledoc """
    HTTP or protocol error returned by `EntroQ.Client`.
    """

    defexception [:status, :body, message: ""]

    @type t :: %__MODULE__{
            status: non_neg_integer() | nil,
            body: term(),
            message: String.t()
          }
  end

  @doc """
  Builds a client for an EntroQ HTTP endpoint.
  """
  @spec new(String.t(), keyword()) :: t()
  def new(base_url, opts \\ []) when is_binary(base_url) do
    %__MODULE__{
      base_url: String.trim_trailing(base_url, "/"),
      claimant_id: Keyword.get_lazy(opts, :claimant_id, &random_claimant_id/0),
      headers: normalize_headers(Keyword.get(opts, :headers, [])),
      transport: Keyword.get(opts, :transport, EntroQ.Client.HTTP),
      transport_opts: Keyword.get(opts, :transport_opts, [])
    }
  end

  @doc """
  Returns the client claimant ID.
  """
  @spec claimant_id(t()) :: String.t()
  def claimant_id(%__MODULE__{claimant_id: claimant_id}), do: claimant_id

  @doc """
  Returns the server time in milliseconds since the Unix epoch.
  """
  @spec time(t()) :: {:ok, integer()} | {:error, term()}
  def time(client) do
    with {:ok, data} <- request(client, :get, "/api/v0/time") do
      {:ok, int(data["timeMs"])}
    end
  end

  @doc """
  Lists queue statistics.
  """
  @spec queues(t(), keyword()) :: {:ok, [QueueStats.t()]} | {:error, term()}
  def queues(client, opts \\ []) do
    query =
      []
      |> put_repeated("matchPrefix", Keyword.get(opts, :prefix))
      |> put_repeated("matchExact", Keyword.get(opts, :exact))
      |> put_if("limit", Keyword.get(opts, :limit, 0))

    with {:ok, data} <- request(client, :get, "/api/v0/queues", nil, query) do
      {:ok, Enum.map(data["queues"] || [], &QueueStats.from_json/1)}
    end
  end

  @doc """
  Lists tasks.
  """
  @spec tasks(t(), keyword()) :: {:ok, [Task.t()]} | {:error, term()}
  def tasks(client, opts \\ []) do
    query =
      []
      |> put_if("queue", Keyword.get(opts, :queue, ""))
      |> put_if("limit", Keyword.get(opts, :limit, 0))
      |> put_repeated("taskId", Keyword.get(opts, :task_ids))
      |> put_bool("omitValues", Keyword.get(opts, :omit_values, false))

    with {:ok, data} <- request(client, :get, "/api/v0/tasks", nil, query) do
      {:ok, Enum.map(data["tasks"] || [], &Task.from_json/1)}
    end
  end

  @doc """
  Attempts to claim a task without polling.
  """
  @spec try_claim(t(), String.t() | [String.t()], keyword()) ::
          {:ok, Task.t() | nil} | {:error, term()}
  def try_claim(client, queues, opts \\ []) do
    body =
      %ClaimRequest{
        claimant_id: client.claimant_id,
        queues: List.wrap(queues),
        duration_ms: Keyword.get(opts, :duration_ms, 30_000),
        poll_ms: 0
      }
      |> ClaimRequest.to_json()

    with {:ok, data} <- request(client, :post, "/api/v0/claim", body) do
      case data["task"] do
        nil -> {:ok, nil}
        task -> {:ok, Task.from_json(task)}
      end
    end
  end

  @doc """
  Claims a task, polling server-side according to `:poll_ms`.
  """
  @spec claim(t(), String.t() | [String.t()], keyword()) :: {:ok, Task.t()} | {:error, term()}
  def claim(client, queues, opts \\ []) do
    body =
      %ClaimRequest{
        claimant_id: client.claimant_id,
        queues: List.wrap(queues),
        duration_ms: Keyword.get(opts, :duration_ms, 30_000),
        poll_ms: Keyword.get(opts, :poll_ms, 5_000)
      }
      |> ClaimRequest.to_json()

    with {:ok, data} <- request(client, :post, "/api/v0/claim/wait", body) do
      case data["task"] do
        nil -> {:error, %Error{message: "claim returned no task"}}
        task -> {:ok, Task.from_json(task)}
      end
    end
  end

  @doc """
  Atomically applies a task/doc modification.
  """
  @spec modify(t(), Modification.t(), keyword()) :: {:ok, ModifyResult.t()} | {:error, term()}
  def modify(client, %Modification{} = modification, opts \\ []) do
    claimant_id = Keyword.get(opts, :unsafe_claimant_id, client.claimant_id)

    with {:ok, data} <-
           request(
             client,
             :post,
             "/api/v0/modify",
             Modification.to_json(modification, claimant_id)
           ) do
      {:ok, ModifyResult.from_json(data)}
    end
  end

  @doc """
  Lists docs in a namespace, optionally filtered by key range or IDs.
  """
  @spec docs(t(), keyword()) :: {:ok, [Doc.t()]} | {:error, term()}
  def docs(client, opts \\ []) do
    query =
      %DocQuery{
        namespace: Keyword.get(opts, :namespace, ""),
        key_start: Keyword.get(opts, :key_start, ""),
        key_end: Keyword.get(opts, :key_end, ""),
        key_exact: Keyword.get(opts, :key_exact, ""),
        ids: Keyword.get(opts, :ids, []),
        limit: Keyword.get(opts, :limit, 0),
        omit_values: Keyword.get(opts, :omit_values, false)
      }
      |> DocQuery.to_query_params()

    with {:ok, data} <- request(client, :get, "/api/v0/docs", nil, query) do
      {:ok, Enum.map(data["docs"] || [], &Doc.from_json/1)}
    end
  end

  @doc """
  Atomically claims all docs sharing `namespace` and `key`.
  """
  @spec claim_docs(t(), DocClaim.t() | keyword()) :: {:ok, [Doc.t()]} | {:error, term()}
  def claim_docs(client, claim) do
    claim = DocClaim.new(claim)

    body =
      %ClaimDocsRequest{
        claim_query: %{claim | claimant: claim.claimant || client.claimant_id}
      }
      |> ClaimDocsRequest.to_json()

    with {:ok, data} <- request(client, :post, "/api/v0/docs/claim", body) do
      {:ok, Enum.map(data["docs"] || [], &Doc.from_json/1)}
    end
  end

  @doc """
  Lists doc namespace statistics.
  """
  @spec namespace_stats(t(), keyword()) :: {:ok, [NamespaceStat.t()]} | {:error, term()}
  def namespace_stats(client, opts \\ []) do
    query =
      []
      |> put_repeated("matchPrefix", Keyword.get(opts, :prefix))
      |> put_repeated("matchExact", Keyword.get(opts, :exact))
      |> put_if("limit", Keyword.get(opts, :limit, 0))

    with {:ok, data} <- request(client, :get, "/api/v0/namespaces/stats", nil, query) do
      {:ok, Enum.map(data["namespaces"] || [], &NamespaceStat.from_json/1)}
    end
  end

  @doc false
  def request(client, method, path, body \\ nil, query \\ []) do
    url = client.base_url <> path <> query_string(query)
    encoded_body = if is_nil(body), do: nil, else: Jason.encode!(body)
    headers = [{"content-type", "application/json"} | client.headers]

    case client.transport.request(method, url, headers, encoded_body, client.transport_opts) do
      {:ok, %{status: status, body: response_body}} when status in 200..299 ->
        decode_success(response_body)

      {:ok, %{status: status, body: response_body}} ->
        decode_error(status, response_body)

      {:error, reason} ->
        {:error, reason}
    end
  end

  defp decode_success(nil), do: {:ok, %{}}
  defp decode_success(""), do: {:ok, %{}}

  defp decode_success(body) do
    case Jason.decode(body) do
      {:ok, data} -> {:ok, data}
      {:error, error} -> {:error, %Error{message: "decode response: #{Exception.message(error)}"}}
    end
  end

  defp decode_error(status, body) do
    decoded =
      case Jason.decode(body || "") do
        {:ok, data} -> data
        {:error, _error} -> nil
      end

    if status in [404, 409] and DependencyError.dependency_json?(decoded) do
      {:error, DependencyError.from_json(decoded)}
    else
      message =
        cond do
          is_map(decoded) and is_binary(decoded["message"]) -> decoded["message"]
          is_binary(body) and body != "" -> body
          true -> "EntroQ request failed"
        end

      {:error, %Error{status: status, body: decoded || body, message: message}}
    end
  end

  defp normalize_headers(headers) when is_map(headers), do: Enum.into(headers, [])
  defp normalize_headers(headers), do: headers

  defp query_string([]), do: ""
  defp query_string(query), do: "?" <> URI.encode_query(Enum.reverse(query))

  defp put_if(query, _key, nil), do: query
  defp put_if(query, _key, ""), do: query
  defp put_if(query, _key, 0), do: query
  defp put_if(query, key, value), do: [{key, value} | query]

  defp put_bool(query, _key, false), do: query
  defp put_bool(query, key, true), do: [{key, "true"} | query]

  defp put_repeated(query, _key, nil), do: query
  defp put_repeated(query, _key, []), do: query

  defp put_repeated(query, key, value) when is_list(value) do
    Enum.reduce(value, query, fn item, acc -> put_if(acc, key, item) end)
  end

  defp put_repeated(query, key, value), do: put_if(query, key, value)

  defp random_claimant_id do
    8
    |> :crypto.strong_rand_bytes()
    |> Base.encode16(case: :lower)
  end

  defp int(nil), do: 0
  defp int(value) when is_integer(value), do: value
  defp int(value) when is_binary(value), do: String.to_integer(value)
end
