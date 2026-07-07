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
      docs: docs(),
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
      {:jason, "~> 1.4"},
      {:ex_doc, "~> 0.35", only: :dev, runtime: false}
    ]
  end

  defp package do
    [
      licenses: ["Apache-2.0"],
      links: %{"GitHub" => "https://github.com/shiblon/entroq"}
    ]
  end

  defp docs do
    [
      main: "readme",
      extras: ["README.md"],
      source_url: "https://github.com/shiblon/entroq",
      source_ref: "develop",
      source_url_pattern:
        "https://github.com/shiblon/entroq/blob/develop/clients/elixir/%{path}#L%{line}"
    ]
  end
end
