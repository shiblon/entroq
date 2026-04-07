# EntroQ Authorization with OPA

This document explains how EntroQ's authorization system works, how to configure
it, and how to integrate it with real identity providers (Google, Auth0, Okta, etc.).

## Overview

EntroQ uses [Open Policy Agent (OPA)](https://www.openpolicyagent.org/) for
authorization. The design separates three concerns:

1. **Core logic** (`conf/core/`) - built-in, not meant to be changed. Decides
   whether a request is allowed based on the queues requested and the queues
   allowed for this user.

2. **User identity** (`conf/example/example-entroq-user.rego`) - you must adapt
   this for your IDP. Extracts a username from the incoming JWT.

3. **Permissions** (`conf/example/example-entroq-permissions.rego`) - adapt or
   replace this. Maps usernames to allowed queues, using whatever data source
   you choose.

The example files in `conf/example/` are reference implementations. Copy and
adapt them; do not use them unmodified in production.


## How a Request Flows Through Authorization

When an EntroQ client makes a gRPC call (Claim, Modify, Tasks, etc.), the server:

1. Extracts the `Authorization` header, splits it into `type` and `credentials`.
2. Builds a JSON document describing the request: which queues, which actions.
3. POSTs that document to OPA at the configured URL/path.
4. OPA evaluates the policy and returns `{"result": {"allow": true/false, ...}}`.
5. If `allow` is false, the server returns a PermissionDenied error.

The JSON document sent to OPA (`input`) looks like this:

```json
{
  "authz": {
    "type": "Bearer",
    "credentials": "<the raw JWT string>"
  },
  "queues": [
    {"exact": "/myapp/jobs", "actions": ["CLAIM"]},
    {"exact": "/myapp/results", "actions": ["INSERT"]}
  ]
}
```

Actions are: `CLAIM`, `INSERT`, `DELETE`, `CHANGE`, `READ`, `*` (wildcard).

Queue specs use either `"exact"` (full name match) or `"prefix"` (prefix match).


## Running OPA

OPA runs as a sidecar alongside the EntroQ server. The simplest setup:

```bash
# Download OPA
curl -L -o opa https://openpolicyagent.org/downloads/latest/opa_linux_amd64_static
chmod +x opa

# Run with your policy bundle directory
./opa run --server \
  --addr 0.0.0.0:8181 \
  --bundle /path/to/your/conf/
```

Then tell EntroQ to use it:

```bash
eqpg serve \
  --authz opahttp \
  --opa_url http://localhost:8181 \
  --opa_path /v1/data/entroq/authz \
  ...
```

The `opa_path` corresponds to the Rego `package entroq.authz` declaration in
`core-entroq-authz.rego`. Do not change this path unless you also rename the
package in all Rego files.

In Kubernetes, OPA typically runs as a sidecar container in the same pod as
the EntroQ server, on `localhost:8181`.


## Step 1: Configure User Identity (JWT Validation)

Copy `conf/example/example-entroq-user.rego` and adapt it for your IDP.

The reference implementation uses `io.jwt.decode_verify`, which validates the
signature, expiry, audience, and issuer automatically. It fetches the JWKS
(public keys) from your IDP using `http.send` with OPA's built-in caching.

You control the IDP settings via OPA bundle data (not hardcoded in policy):

**data.json** (part of your OPA bundle):
```json
{
  "entroq": {
    "idp": {
      "jwks_url": "https://www.googleapis.com/oauth2/v3/certs",
      "audience": "your-google-oauth-client-id",
      "issuer":   "https://accounts.google.com"
    }
  }
}
```

### IDP-specific settings

**Google:**
```json
{
  "jwks_url": "https://www.googleapis.com/oauth2/v3/certs",
  "audience": "<your-client-id>.apps.googleusercontent.com",
  "issuer":   "https://accounts.google.com"
}
```

**Auth0:**
```json
{
  "jwks_url": "https://<your-tenant>.auth0.com/.well-known/jwks.json",
  "audience": "https://<your-api-identifier>",
  "issuer":   "https://<your-tenant>.auth0.com/"
}
```

**Okta:**
```json
{
  "jwks_url": "https://<your-domain>/oauth2/default/v1/keys",
  "audience": "api://default",
  "issuer":   "https://<your-domain>/oauth2/default"
}
```

**Azure AD:**
```json
{
  "jwks_url": "https://login.microsoftonline.com/<tenant-id>/discovery/v2.0/keys",
  "audience": "<your-application-client-id>",
  "issuer":   "https://login.microsoftonline.com/<tenant-id>/v2.0"
}
```

The `username` rule in the user package is what the core policy uses to
identify who is making the request. It must resolve to a string. If it is
undefined (e.g. invalid token, wrong audience), the request is denied.

The claim used as username (`payload.sub`) is the standard OIDC subject - a
stable, unique identifier for the user within that IDP. You could also use
`payload.email` if your IDP includes it and you prefer human-readable
identifiers. Just be aware that email addresses can change.


## Step 2: Configure Permissions

The reference implementation in `example-entroq-permissions.rego` maps users
and roles to allowed queues using a policy data document. The shape of that
document:

```json
{
  "entroq": {
    "policy": {
      "users": [
        {
          "name": "user-subject-from-jwt",
          "roles": ["editor"],
          "queues": [
            {"exact": "/personal/myqueue", "actions": ["ALL"]}
          ]
        }
      ],
      "roles": [
        {
          "name": "editor",
          "queues": [
            {"prefix": "/shared/", "actions": ["CLAIM", "INSERT", "DELETE"]}
          ]
        },
        {
          "name": "*",
          "queues": [
            {"prefix": "/public/", "actions": ["READ", "CLAIM"]}
          ]
        }
      ]
    }
  }
}
```

The `"*"` role applies to all authenticated users automatically.

Every authenticated user also gets implicit `ALL` access to queues under
`/ns=user/<username>/` - a personal namespace they own outright.

### Queue specs

A queue spec has either `"exact"` or `"prefix"`, plus `"actions"`:

```json
{"exact": "/myapp/jobs",       "actions": ["CLAIM", "DELETE"]}
{"prefix": "/myapp/",          "actions": ["ALL"]}
{"prefix": "/shared/reports/", "actions": ["READ"]}
```

Actions: `CLAIM`, `INSERT`, `DELETE`, `CHANGE`, `READ`, `ALL` (wildcard).


## Providing Policy Data to OPA

There are two main ways to get the users/roles data into OPA:

### Option A: Bundle (recommended)

A bundle is a directory (or tarball) of `.rego` files and a `data.json`.
OPA can load bundles from the filesystem or from an HTTP endpoint (S3, GCS, etc.)
that it polls periodically.

```
conf/
  core/
    core-entroq-authz.rego
    core-entroq-queues.rego
  example/
    example-entroq-user.rego       <- your adapted version
    example-entroq-permissions.rego
  data.json                        <- users, roles, IDP config
```

Run OPA pointing at this directory:
```bash
opa run --server --bundle ./conf/
```

To update users/roles without restarting OPA, push a new bundle to the HTTP
bundle endpoint or update the file and send OPA a SIGINT (it reloads bundles).

### Option B: Data API (simpler for dynamic updates)

Push data directly to OPA's REST API at runtime:

```bash
# Set the policy data document
curl -X PUT http://localhost:8181/v1/data/entroq/policy \
  -H "Content-Type: application/json" \
  -d @policy.json

# Update just the users list
curl -X PUT http://localhost:8181/v1/data/entroq/policy/users \
  -H "Content-Type: application/json" \
  -d '[{"name": "alice", "queues": [...]}]'
```

This is convenient but data is lost on OPA restart unless you also use a
bundle for the base data.


## Testing Your Policies

OPA has a built-in test runner. To run the tests that ship with this package:

```bash
opa test ./conf/ -v
```

When writing your own tests, mock `data.entroq.user.username` and
`data.entroq.permissions.allowed_queues` directly rather than going through
JWT validation - that keeps policy logic tests independent of any real IDP:

```rego
test_my_user_can_claim {
  allow
  with data.entroq.user.username as "alice"
  with data.entroq.permissions.allowed_queues as [
    {"prefix": "/myapp/", "actions": ["ALL"]}
  ]
  with input.queues as [
    {"exact": "/myapp/jobs", "actions": ["CLAIM"]}
  ]
}
```


## Replacing the Permissions Implementation

The permissions package (`entroq.permissions`) is a seam - you can replace it
entirely. The only contract the core expects is:

```rego
# Package entroq.permissions must export:
allowed_queues[q] { ... }  # partial rule; q is a queue spec object
```

You could implement this by:
- Calling an external API with `http.send` (e.g. your own RBAC service)
- Reading from OPA bundle data (as the example does)
- Deriving permissions from JWT claims directly (no data document needed)

Example: derive allowed queues entirely from JWT claims, no data document:

```rego
package entroq.permissions

import data.entroq.user

# Allow access to the user's personal namespace.
allowed_queues[q] {
    q := {
        "prefix": concat("", ["/ns=user/", user.username, "/"]),
        "actions": ["ALL"]
    }
}

# Allow access to any org prefix embedded in the JWT as a custom claim.
allowed_queues[q] {
    org := user.jwt_body.org_id
    q := {
        "prefix": concat("", ["/org/", org, "/"]),
        "actions": ["ALL"]
    }
}
```

This approach avoids any external data synchronization - permissions flow
entirely from the validated token.


## Security Notes

- `io.jwt.decode_verify` validates the signature, expiry, audience, and issuer.
  Never use `io.jwt.decode` (no verification) in production.
- OPA's `http.send` with `"cache": true` caches JWKS responses. The cache
  respects HTTP cache headers from the IDP. If you need faster key rotation,
  set `"force_cache_duration_seconds"` to a shorter value.
- The OPA HTTP API has no authentication by default. In production, either
  run it on localhost only (sidecar), or configure OPA's built-in bearer token
  auth, or put it behind a network policy.
- If OPA is unreachable, `Authorize` returns an error and the request is denied.
  This is fail-closed behavior.
