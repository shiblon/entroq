defmodule EntroQ.Client.HTTP do
  @moduledoc false

  @type method :: :get | :post

  @spec request(method(), String.t(), [{String.t(), String.t()}], String.t() | nil, keyword()) ::
          {:ok, %{status: non_neg_integer(), body: String.t()}} | {:error, term()}
  def request(method, url, headers, body, opts \\ []) do
    timeout = Keyword.get(opts, :timeout, 30_000)

    http_opts = [
      timeout: timeout,
      connect_timeout: Keyword.get(opts, :connect_timeout, timeout)
    ]

    opts = [body_format: :binary]
    request = request_tuple(method, url, headers, body)

    case :httpc.request(method, request, http_opts, opts) do
      {:ok, {{_version, status, _reason}, _headers, response_body}} ->
        {:ok, %{status: status, body: response_body}}

      {:error, reason} ->
        {:error, reason}
    end
  end

  defp request_tuple(:get, url, headers, nil) do
    {String.to_charlist(url), charlist_headers(headers)}
  end

  defp request_tuple(_method, url, headers, body) do
    {String.to_charlist(url), charlist_headers(headers), ~c"application/json", body || ""}
  end

  defp charlist_headers(headers) do
    Enum.map(headers, fn {key, value} -> {String.to_charlist(key), String.to_charlist(value)} end)
  end
end
