# EntroQ Authorization with OPA

This document explains how EntroQ's authorization system works, how to configure
it, and how to integrate it with real identity providers (Google, Auth0, Okta, etc.).

**Scope:** This authorization layer applies only to the gRPC and HTTP/JSON
(Connect) server endpoints (`eqpg serve`, `eqmemsvc`). It does not apply to
direct backend connections (e.g. connecting a Go client directly to PostgreSQL).
See Security Notes for details.

## Overview

EntroQ uses [Open Policy Agent (OPA)](https://www.openpolicyagent.org/) for
authorization. The design separates three concerns:

1. **Core logic** (`conf/core/`) - built-in, do not modify. Decides whether a
   request is allowed based on the queues requested and the queues allowed for
   this user.

2. **User identity** (`conf/providers/entroq/user/`) - resolves a username from
   the incoming JWT. The built-in OIDC provider works with any IDP that serves a
   standard JWKS endpoint (Google, Auth0, Okta, Azure AD, Keycloak, and others).
   Configure it via `data.json`; no code changes needed for standard IDPs.

3. **Permissions** (`conf/providers/entroq/permissions/`) - maps usernames to
   allowed queues and namespaces. The built-in implementation uses a policy data
   document (users, roles, queue specs). Replace or extend it if you need a
   different data source (e.g. an external RBAC service).

The files in `conf/providers/` are production-ready. Configure them via
`data.json`; only replace them if your IDP or permission model is non-standard.

### Directory layout

```
conf/
  core/                          <- do not modify
    entroq/authz/                <- package entroq.authz  (allow/deny logic)
    entroq/queues/               <- package entroq.queues (queue matching)
    entroq/namespaces/           <- package entroq.namespaces
  providers/                     <- configure via data.json
    entroq/user/                 <- package entroq.user   (JWT -> username)
    entroq/permissions/          <- package entroq.permissions (username -> queues)
  tests/
    core/                        <- tests for core logic
    providers/                   <- tests for provider logic
```


## How a Request Flows Through Authorization

Both the gRPC and HTTP/JSON (Connect) endpoints share the same authorization
implementation. All authz checks -- including claimant binding -- behave
identically regardless of which transport the client uses.

When an EntroQ client makes a call (Claim, Modify, Tasks, etc.), the server:

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

OPA runs as a sidecar alongside the EntroQ server. Tell EntroQ to use it with:

```bash
eqpg serve \
  --authz opahttp \
  --opa_url http://localhost:8181 \
  --opa_path /v1/data/entroq/authz \
  ...
```

The `opa_path` corresponds to `package entroq.authz` in the core Rego files.
Do not change it unless you rename that package everywhere.

### Docker (recommended for most deployments)

The `examples/authz/` directory provides a ready-to-run docker-compose setup.
The general pattern is three mounts into the OPA container:

| Mount | Source | Purpose |
|---|---|---|
| `/etc/opa/core` | `conf/core/` from this repo | Core logic -- do not modify |
| `/etc/opa/providers` | `conf/providers/` from this repo | OIDC user + permissions -- do not modify |
| `/etc/opa/local` | your own directory | `data.json` (IDP config, users, roles) |

Your `local/` directory is the only thing you own and edit. It needs one file:

```
local/
  data.json    <- IDP settings + users/roles policy
```

For a real IDP, `data.json` looks like:

```json
{
  "entroq": {
    "idp": {
      "jwks_url": "https://your-idp/.well-known/jwks.json",
      "audience": "your-audience",
      "issuer":   "https://your-idp/"
    },
    "policy": {
      "users": [...],
      "roles": [...]
    }
  }
}
```

OPA's `--watch` flag (used in the example compose file) reloads policy
automatically when files change -- editing `data.json` and saving is enough
to update users and roles without restarting anything.

For durable config on a shared or persistent volume, the recommended approach
is to copy your `local/` directory to the volume at deploy time (e.g. via an
init container or a startup script), then mount it into OPA. The `core/` and
`providers/` directories should be treated as read-only and sourced directly
from the container image or repo -- they are updated only when you upgrade
EntroQ.

### Kubernetes

OPA typically runs as a sidecar container in the same pod as the EntroQ server,
communicating on `localhost:8181`.

For zero-downtime policy updates, use OPA's bundle server support instead of
file mounts. OPA polls an HTTP endpoint (S3, GCS, an OPA bundle server, etc.)
for updated bundles and reloads automatically:

```yaml
# opa-config.yaml
services:
  bundleserver:
    url: https://your-bundle-server

bundles:
  entroq:
    service: bundleserver
    resource: /entroq-policy.tar.gz
    polling:
      min_delay_seconds: 60
      max_delay_seconds: 120
```

```bash
opa run --server --config-file opa-config.yaml
```

With this approach, pushing a new bundle to the bundle server is enough to
update policy cluster-wide with no pod restarts.


## Step 1: Configure User Identity (JWT Validation)

The built-in OIDC provider (`conf/providers/entroq/user/oidc-entroq-user.rego`)
works with any standard OIDC IDP out of the box. It uses `io.jwt.decode_verify`,
which validates the signature, expiry, audience, and issuer automatically, and
fetches the JWKS (public keys) from your IDP using `http.send` with OPA's
built-in caching.

Configure it entirely via OPA bundle data -- no code changes needed:

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

The `name` rule in `package entroq.user` is what the core policy uses to
identify who is making the request. It must resolve to a string. If it is
undefined (e.g. invalid token, wrong audience), the request is denied.

The claim used as the username is `payload.sub` -- the standard OIDC subject, a
stable, unique identifier for the user within that IDP. You could also use
`payload.email` if your IDP includes it and you prefer human-readable
identifiers, but be aware that email addresses can change.


## Step 2: Configure Permissions

The built-in permissions implementation (`conf/providers/entroq/permissions/`)
maps users and roles to allowed queues using a policy data document. The shape
of that document:

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
  core/                            <- do not modify
    entroq/authz/
    entroq/queues/
    entroq/namespaces/
  providers/                       <- configure via data.json
    entroq/user/
    entroq/permissions/
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

When writing your own tests, mock `data.entroq.user.name` and
`data.entroq.permissions.allowed_queues` directly rather than going through
JWT validation -- that keeps policy logic tests independent of any real IDP:

```rego
test_my_user_can_claim if {
  allow
    with data.entroq.user.name as "alice"
    with data.entroq.permissions.allowed_queues as [
      {"prefix": "/myapp/", "actions": ["ALL"]}
    ]
    with data.entroq.permissions.allowed_namespaces as set()
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
allowed_queues contains q if { ... }     # partial rule; q is a queue spec object
allowed_namespaces contains n if { ... } # partial rule; n is a namespace spec object
```

You could implement this by:
- Calling an external API with `http.send` (e.g. your own RBAC service)
- Reading from OPA bundle data (as the built-in implementation does)
- Deriving permissions from JWT claims directly (no data document needed)

Example: derive allowed queues entirely from JWT claims, no data document:

```rego
package entroq.permissions

import rego.v1

import data.entroq.user

# Allow access to the user's personal namespace.
allowed_queues contains q if {
    q := {
        "prefix": concat("", ["/users/", user.name, "/"]),
        "actions": ["ALL"],
    }
}

allowed_namespaces contains n if {
    n := {
        "prefix": concat("", ["/users/", user.name, "/"]),
        "actions": ["ALL"],
    }
}
```

This approach avoids any external data synchronization - permissions flow
entirely from the validated token.


## Claimant Identity Binding

EntroQ tasks carry a claimant ID -- a string identifying which process currently
holds a task. By default each client generates a random ID at startup. In
authenticated deployments you can bind the claimant to the authenticated identity
to prevent impersonation.

### Composite claimant IDs

The recommended format for authenticated workers is `<sub>#<nonce>`, where:

- `sub` is the JWT subject claim (stable across token refreshes, unique per service account)
- `nonce` is a per-process random ID (prevents within-identity process stomping)

Use `entroq.MustClaimantFromSub(token)` to generate this at startup:

```go
eq, err := entroq.New(ctx, opener,
    entroq.WithClaimantID(entroq.MustClaimantFromSub(myToken)),
)
```

### OPA enforcement

The core policy checks `input.claimant_id` against `user.name`. If the claimant
ID does not start with `user.name + "#"`, the request is denied. This prevents
a worker from claiming tasks under another identity's name.

The check only fires when both sides are present: unauthenticated deployments
(no `user.name`) and requests with an empty `claimant_id` pass through unchanged.

### Admin bypass

Admins sometimes need to operate under another process's claimant ID (e.g. for
forced deletion). The built-in permissions implementation defines `is_admin` for
users with the `"admin"` role, which bypasses the claimant check:

```json
{
  "entroq": {
    "policy": {
      "users": [
        {"name": "admin-subject", "roles": ["admin"], "queues": []}
      ]
    }
  }
}
```

### Protection levels

| Deployment | Claimant protection |
|---|---|
| Unauthenticated | None -- any claimant ID accepted |
| Authenticated, shared service account | Cross-identity spoofing blocked; within-identity accidents blocked by nonce; within-identity malice is a known gap |
| Authenticated, per-workload identity (e.g. k8s workload identity) | Full isolation -- each workload has a distinct sub |


## Security Notes

- **Direct backend connections bypass this layer entirely.** If a Go client
  connects directly to PostgreSQL (without going through `eqpg serve`), no OPA
  checks apply. Queue-level restrictions would need to be enforced via PostgreSQL
  row-level security on the tasks table. For multi-tenant deployments, always
  route clients through the gRPC or HTTP server.
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
