defmodule EntroQ.MixProject do
  use Mix.Project

  def project do
    [
      app: :entroq,
      version: "0.1.0",
      elixir: "~> 1.17",
      start_permanent: Mix.env() == :prod,
      description: "Elixir client for the EntroQ task queue.",
      package: package(),
      deps: deps()
    ]
  end

  def application do
    [
      extra_applications: [:inets, :logger, :ssl]
    ]
  end

  defp deps do
    [
      {:jason, "~> 1.4"}
    ]
  end

  defp package do
    [
      licenses: ["MIT"],
      links: %{"GitHub" => "https://github.com/shiblon/entroq"}
    ]
  end
end
