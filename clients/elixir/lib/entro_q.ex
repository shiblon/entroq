defmodule EntroQ do
  @moduledoc """
  Elixir client for EntroQ.

  EntroQ is a task queue with an atomic task/document modification primitive.
  This package exposes the low-level HTTP/JSON RPC client through
  `EntroQ.Client` and the race-safe worker loop through `EntroQ.Worker`.
  """

  @doc """
  Creates a new HTTP client.

  This is a small convenience wrapper around `EntroQ.Client.new/2`.
  """
  defdelegate new(base_url, opts \\ []), to: EntroQ.Client
end
