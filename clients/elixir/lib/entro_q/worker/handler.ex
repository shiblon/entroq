defmodule EntroQ.Worker.Handler do
  @moduledoc false

  defstruct module: nil, take_docs: nil, perform: nil, finish: nil

  def new(%__MODULE__{} = handler), do: handler

  def new(module) when is_atom(module) do
    %__MODULE__{module: module}
  end

  def new(opts) when is_list(opts) do
    %__MODULE__{
      take_docs: Keyword.get(opts, :take_docs, fn _task -> [] end),
      perform: Keyword.fetch!(opts, :perform),
      finish: Keyword.get(opts, :finish, fn _task, _docs, _payload -> :ok end)
    }
  end

  def take_docs(%__MODULE__{module: module}, task) when is_atom(module) and not is_nil(module) do
    if function_exported?(module, :take_docs, 1) do
      normalize_take_docs(module.take_docs(task))
    else
      {:ok, []}
    end
  end

  def take_docs(%__MODULE__{take_docs: take_docs}, task) when is_function(take_docs, 1) do
    normalize_take_docs(take_docs.(task))
  end

  def perform(%__MODULE__{module: module}, task, docs)
      when is_atom(module) and not is_nil(module) do
    module.perform(task, docs)
  end

  def perform(%__MODULE__{perform: perform}, task, docs) when is_function(perform, 2) do
    perform.(task, docs)
  end

  def finish(%__MODULE__{module: module}, task, docs, payload)
      when is_atom(module) and not is_nil(module) do
    if function_exported?(module, :finish, 3) do
      module.finish(task, docs, payload)
    else
      :ok
    end
  end

  def finish(%__MODULE__{finish: finish}, task, docs, payload) when is_function(finish, 3) do
    finish.(task, docs, payload)
  end

  defp normalize_take_docs({:ok, claims}), do: {:ok, claims}
  defp normalize_take_docs({:error, reason}), do: {:error, reason}
  defp normalize_take_docs(claims) when is_list(claims), do: {:ok, claims}
end
