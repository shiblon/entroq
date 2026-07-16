# Dynamic configuration example

A self-contained demo of sharing live configuration through EntroQ. It runs an
in-memory (`eqmem`) instance in one process, with three queues — `/config`,
`/work`, and `/results` — and a worker whose behavior (a multiplier) is driven by
a config value it reads from the `/config` queue, showing how workers can pick up
configuration changes without a restart.

No external services are needed; it uses the in-memory backend. Run it from this
directory:

```sh
go run .
```
