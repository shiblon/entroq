defmodule EntroQ.ClientTest do
  use ExUnit.Case, async: true

  alias EntroQ.{Client, DependencyError}

  defmodule Transport do
    def request(method, url, headers, body, opts) do
      send(Keyword.fetch!(opts, :test_pid), {:request, method, url, headers, body})
      {:ok, Keyword.fetch!(opts, :response)}
    end
  end

  test "generates and reuses claimant IDs" do
    client = Client.new("http://localhost:9100")

    assert Client.claimant_id(client) =~ ~r/^[0-9a-f]{16}$/
    assert Client.claimant_id(client) == Client.claimant_id(client)
  end

  test "try_claim encodes the JSON request" do
    task = %{
      "id" => "t1",
      "version" => 1,
      "queue" => "q",
      "atMs" => "0",
      "claimantId" => "cid",
      "value" => %{"hello" => "world"},
      "createdMs" => "0",
      "modifiedMs" => "0",
      "claims" => 1,
      "attempt" => 0,
      "err" => ""
    }

    client =
      Client.new("http://localhost:9100/",
        claimant_id: "worker-1",
        transport: Transport,
        transport_opts: [
          test_pid: self(),
          response: %{status: 200, body: Jason.encode!(%{"task" => task})}
        ]
      )

    assert {:ok, claimed} = Client.try_claim(client, ["q"], duration_ms: 10_000)
    assert claimed.id == "t1"
    assert claimed.value == %{"hello" => "world"}

    assert_receive {:request, :post, "http://localhost:9100/api/v0/claim", headers, body}
    assert {"content-type", "application/json"} in headers

    assert Jason.decode!(body) == %{
             "claimantId" => "worker-1",
             "queues" => ["q"],
             "durationMs" => "10000",
             "pollMs" => "0"
           }
  end

  test "parses dependency errors from JSON details" do
    body =
      Jason.encode!(%{
        "message" => "conflict",
        "details" => [
          %{"type" => "DELETE", "id" => %{"id" => "t1", "version" => 2, "queue" => "q"}},
          %{
            "type" => "CLAIM",
            "docId" => %{"namespace" => "ns", "id" => "d1", "version" => 4}
          },
          %{"type" => "DETAIL", "msg" => "stale version"}
        ]
      })

    client =
      Client.new("http://localhost:9100",
        transport: Transport,
        transport_opts: [test_pid: self(), response: %{status: 409, body: body}]
      )

    assert {:error, %DependencyError{} = error} = Client.tasks(client, queue: "q")
    assert error.message == "stale version"
    assert [%EntroQ.TaskID{id: "t1", version: 2, queue: "q"}] = error.deletes
    assert [%EntroQ.DocID{namespace: "ns", id: "d1", version: 4}] = error.doc_claims
  end
end
