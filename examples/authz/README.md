# EntroQ authentication and authorization example

This Compose stack demonstrates the verified-principal boundary with a local
RSA key:

| Service | Role |
|---|---|
| PostgreSQL | task storage |
| EntroQ | verifies JWTs and sends verified principals to OPA |
| OPA | evaluates user/role queue policy |

## Run it

From `examples/authz`:

```bash
docker compose up --build

# In another terminal:
python -m pip install requests pyjwt cryptography
python client.py
python test.py
```

The client signs tokens for `alice` and `bob` with `example-key.pem`. EntroQ
loads the matching public key from `jwks.json`, verifies signature, issuer,
audience, and time claims, then asks OPA to authorize the verified subject.
OPA's `data.json` contains only users, roles, and grants.

The example intentionally uses `--auth_jwks_file`. A production service would
normally use:

```text
--authn=jwt
--auth_jwks_url=https://issuer.example/.well-known/jwks.json
--auth_issuer=https://issuer.example/
--auth_audience=entroq-api
--authz=opahttp
```

## Policy changes

Edit `opa/data.json`, then restart OPA if its file watcher does not observe the
bind-mount change:

```bash
docker compose restart opa
```

Policy changes take effect independently of EntroQ's verified-token cache,
because authorization decisions are never cached.

## Rotate the example key

```bash
python keygen.py
docker compose restart entroq
```

`keygen.py` replaces `example-key.pem`, `jwks.json`, and the example policy.
The EntroQ restart reloads the file immediately; without a restart it reloads
after the configured JWKS cache lifetime.

In production, remove both local key files and obtain tokens from the configured
identity provider.
