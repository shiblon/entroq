# EntroQ Authorization Example

Shows how to run EntroQ with OPA-based authorization and a JWT bearer token.

## Components

| Service  | Role |
|----------|------|
| postgres | Task storage |
| entroq   | EntroQ service (eqpg), OPA authorization enabled |
| opa      | Open Policy Agent sidecar -- evaluates Rego policy per request |

## Quick start

```sh
# Build and start the stack.
docker compose up --build

# In another terminal, install Python deps and run the example client.
# The JSON/HTTP API is on port 9100; gRPC is on 37706.
pip install requests pyjwt cryptography
python client.py
```

The client generates self-signed JWTs for two users (`alice` and `bob`) and
shows which queue operations each is allowed or denied based on the policy in
`opa/data.json`.

The schema is initialized automatically on first start (`--init_schema`), so no
separate setup step is needed.

## Running the integration test

```sh
python test.py
```

Exits 0 if all assertions pass, 1 on any failure.

## How it works

1. The client signs a JWT with the private key in `example-key.pem`.
2. The JWT is sent as `Authorization: Bearer <token>` on every HTTP request.
3. EntroQ forwards the header to OPA before processing each operation.
4. OPA evaluates `data.entroq.authz.allow`:
   - `opa/user.rego` verifies the JWT signature against the inline JWKS and
     extracts the `sub` claim as the username.
   - `pkg/authz/opadata/conf/example/example-entroq-permissions.rego` maps
     the username to allowed queues via `opa/data.json`.
   - `pkg/authz/opadata/conf/core/` contains the core queue-matching logic.
5. EntroQ allows or denies the operation based on OPA's decision.

## Customizing the policy

Edit `opa/data.json` to add users, roles, and queue permissions.

> **macOS note:** OPA's `--watch` flag uses inotify, which does not propagate
> reliably through Docker Desktop's VM layer. After editing any `.rego` or
> `data.json` file, reload OPA manually:
> ```sh
> docker compose restart opa
> ```

## Rotating keys

```sh
python keygen.py
```

Regenerates `example-key.pem` and updates `opa/data.json` with the new
public key. Restart OPA after rotating:

```sh
docker compose restart opa
```

## Using a real IDP

Replace `opa/user.rego` with the reference implementation at
`pkg/authz/opadata/conf/example/example-entroq-user.rego`, which fetches
JWKS from a live URL. Update `opa/data.json`:

```json
{
  "entroq": {
    "idp": {
      "jwks_url": "https://your-idp/.well-known/jwks.json",
      "audience": "your-client-id",
      "issuer": "https://your-idp"
    }
  }
}
```

Remove `example-key.pem` and replace `make_token()` in `client.py` with your
IDP's token acquisition flow (OAuth2 device flow, client credentials, etc.).
Then restart OPA.
