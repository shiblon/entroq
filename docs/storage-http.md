# Eqmr object-store HTTP protocol

Eqmr object-store drivers are long-running child processes that expose ordinary
HTTP. The protocol does not prescribe its byte transport: a deployment may use
stdin/stdout, a Unix-domain socket, an inherited connected socket, or an
inherited listener. Transport selection and process sandboxing are local worker
configuration and never appear in durable run state.

The protocol boundary is deliberately below the MapReduce record layer. Eqmr
owns partitioning, sorting, the immutable-run encoding, footer metadata,
checksums, and validation. A driver stores, opens, and deletes opaque byte
objects; it never interprets records.

Protocol version 1 identifies itself as `entroq.eqmr.object-store/1`. All small
JSON control requests and responses are limited to 64 KiB. Object bodies are
unbounded streams with ordinary HTTP backpressure.

## Immutable-run byte format

Every object written by eqmr is one versioned immutable run:

1. The eight-byte header `45 51 4d 52 52 55 4e 01` (`EQMRRUN` followed by
   version byte 1).
2. Zero or more record frames. A frame starts with tag byte `01`, followed by
   unsigned varint lengths for primary key, secondary key, and value, followed
   by those three byte strings in the same order.
3. One footer frame. It starts with tag byte `00`, followed by the unsigned
   varint record count and an eight-byte big-endian xxHash64 checksum.
4. End of stream. Bytes after the checksum are invalid.

The checksum covers every byte from the header through the encoded footer
count; it does not cover the checksum itself. The pointer published in EntroQ
also carries the record count, and readers require the pointer, footer, and
number of decoded frames to agree. A record's three fields may total at most 64
MiB. Empty fields are valid.

Records are sorted by `(primary, secondary, value)` before encoding. The object
driver treats this format as opaque and therefore needs no codec implementation.

## Information

`GET /v1/info` returns:

```json
{
  "protocol": "entroq.eqmr.object-store/1",
  "driver": "example.com/acme/object-store/v1",
  "identity": "sha256:credential-independent-store-identity"
}
```

`driver` names the implementation and its descriptor compatibility version.
`identity` identifies the endpoint, bucket, root, or equivalent durable storage
configuration without including credentials. Every worker participating in a
run must observe the identity recorded when that run was created.

## Put

`PUT /v1/objects/{object-id}` streams an `application/octet-stream` request
body. Eqmr chooses a globally unique, URL-safe object ID before starting the
request.

The driver must make the object visible atomically and durably by its backend's
rules before responding. A successful first write returns `201 Created`; an
idempotent retry of the same ID and bytes may return `200 OK`. Both carry an
opaque non-null JSON reference:

```json
{"ref":{"provider_key":"runs/2026/09/25/abc"}}
```

The driver returns `409 Conflict` if the object ID already names different
bytes. A request that ends before its body is complete must not publish a
partial object.

The response reference is persisted verbatim. Eqmr does not inspect it, and a
driver must continue accepting references written by every compatible version
of that driver.

## Open

`POST /v1/open` sends a persisted reference:

```json
{"ref":{"provider_key":"runs/2026/09/25/abc"}}
```

`200 OK` returns the object as a streamed `application/octet-stream` body.
`404 Not Found` means the reference names no object. Eqmr validates the run
format, record count, and checksum while consuming the body.

## Delete

`POST /v1/delete` sends the same reference envelope. Deletion is idempotent:
the driver returns `204 No Content` whether it removed the object now or it was
already absent.

## Errors

Non-success responses should carry a small JSON body:

```json
{"code":"object_conflict","message":"object ID already has different contents"}
```

The code is driver-stable diagnostic data; the message is for operators. HTTP
status determines disposition. `429` and `5xx` responses are retryable. Other
`4xx` responses report a permanent request or reference problem unless an
operation documents otherwise.

## Credentials and isolation

Neither requests nor durable store descriptors contain credentials. The child
may receive a narrowly scoped capability through an inherited descriptor, use
workload identity, or contact a credential broker. On Linux, an operator may
launch the child under bubblewrap to clear its environment, restrict its
filesystem and inherited descriptors, tie its lifetime to the worker, and grant
network access only when the backing store needs it.

Executable selection, arguments, sandbox policy, and credential sources are
operator configuration. A task can select only a store descriptor already
allowed by its worker; task data can never select an arbitrary command.
