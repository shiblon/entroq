# EQLink streaming smoke test

This opt-in integration test exercises EQLink's bidirectional HTTP transport in
a disposable three-pod k3d mesh. Both sidecars use the production `eqlink run`
shape:

```text
probe + EQLink sender -> EntroQ -> EQLink receiver + duplex echo server
```

It verifies concurrent streams and frames larger than the probe's read buffer,
then injects abrupt receiver and sender crashes and an EntroQ outage. Active
HTTP connections may be lost, but must not report false success; replacement
pods must accept fresh streams. EQLink's liveness timeout is shortened to nine
seconds for this test only.

Run it explicitly from the repository root:

```bash
./scripts/eqlink-smoke.sh
```

Prerequisites are Docker, k3d, kubectl, and Go. The harness uses one k3s server
with no agents or load balancer, disables bundled addons it does not need, caps
the server container at 2 GiB, and does not change the default kubeconfig or
context. It uses the in-memory EntroQ backend because this is a transport test,
not a backend durability test.

The cluster, local scratch image, generated binaries, build cache, and private
kubeconfig are removed on exit. Diagnostic logs remain under the ignored
`benchmarks/mesh/results/` directory. Set `KEEP_CLUSTER=1` only when the cluster
must remain available for manual inspection; the output names the exact cleanup
command.
