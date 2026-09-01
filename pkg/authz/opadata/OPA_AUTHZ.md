# EntroQ authorization with OPA

EntroQ authenticates callers before asking OPA to authorize their requested
queue and document operations. OPA receives verified identity facts, not bearer
credentials.

## Trust boundary

The EntroQ service owns authentication:

1. Read the `Authorization` metadata from the gRPC or Connect request.
2. Require a Bearer JWT.
3. Verify its asymmetric signature against the configured JWKS.
4. Require the configured issuer, at least one configured audience, expiration,
   and all applicable issued-at/not-before constraints.
5. Construct `authn.VerifiedPrincipal` from the verified claims.
6. Ask OPA whether that principal may perform the named operations.

OPA owns authorization:

1. Map the verified principal into the environment's local identity model.
2. Derive queue and namespace grants.
3. Enforce claimant binding.
4. Return an explicit allow or deny decision.

Clients cannot provide `input.principal`; it is not part of the EntroQ wire
request. The OPA Data API should still be treated as an internal interface. A
direct caller can manufacture an OPA query, but an OPA response grants nothing
unless the EntroQ enforcement point made and honored that query.

## Service configuration

Authentication and authorization are enabled together:

```bash
eqpg serve \
  --authn=jwt \
  --auth_jwks_url=https://issuer.example/.well-known/jwks.json \
  --auth_issuer=https://issuer.example/ \
  --auth_audience=entroq-api \
  --authz=opahttp \
  --opa_url=http://localhost:8181
```

Use `--auth_jwks_file=/path/to/jwks.json` instead of `--auth_jwks_url` for a
local key set. Exactly one source is required. A local file is convenient for
development; production deployments normally use an HTTPS JWKS endpoint.

Authentication flags:

| Flag | Default | Meaning |
|---|---:|---|
| `--authn` | `none` | `jwt` enables verified JWT principals |
| `--auth_jwks_url` | empty | JWKS HTTP(S) endpoint |
| `--auth_jwks_file` | empty | Local JWKS file; mutually exclusive with URL |
| `--auth_issuer` | empty | Required `iss` claim |
| `--auth_audience` | empty | Required audience; repeat for alternatives |
| `--auth_ca_file` | empty | Additional CA certificates for JWKS HTTPS |
| `--auth_token_cache_ttl` | `30s` | Maximum verified-token cache lifetime; `0` disables |
| `--auth_token_cache_entries` | `4096` | Bounded verified-token LRU size |
| `--auth_jwks_cache_ttl` | `5m` | Key-set cache lifetime |
| `--auth_jwks_refresh_interval` | `5s` | Minimum refresh interval for unknown key IDs |
| `--auth_http_timeout` | `10s` | JWKS request timeout |

Authorization flags remain `--authz=opahttp`, `--opa_url`, and `--opa_path`.
Leaving both `--authn` and `--authz` at `none` runs an open service. Enabling
only one is a configuration error.

## OPA input contract

The default `/v1/data/entroq/authz` decision receives:

```json
{
  "input": {
    "principal": {
      "subject": "alice",
      "issuer": "https://issuer.example/",
      "audience": ["entroq-api"],
      "expires_at": 1788273063,
      "claims": {
        "sub": "alice",
        "iss": "https://issuer.example/",
        "aud": "entroq-api",
        "exp": 1788273063,
        "groups": ["operators"]
      }
    },
    "claimant_id": "alice#worker-7",
    "queues": [
      {"exact": "/shared/inbox", "actions": ["CLAIM", "DELETE"]}
    ],
    "namespaces": [
      {"prefix": "/shared/docs/", "actions": ["READ"]}
    ]
  }
}
```

The bearer token and its digest are never included.

An `entroq.user` provider supplies the local identity. The built-in provider is
deliberately small:

```rego
package entroq.user

import rego.v1

name := input.principal.subject
claims := input.principal.claims
```

Customize this projection when the local username comes from another verified
claim. Do not create a second authentication path in Rego.

An `entroq.permissions` provider supplies:

- `allowed_queues`: queue specifications containing `exact` or `prefix` and
  permitted actions;
- `allowed_namespaces`: equivalent document-namespace specifications;
- `is_admin`: whether claimant binding may be bypassed.

The core policy fails closed when the decision is undefined, the principal has
no grants, a requested resource is unnamed, an action is missing, or a non-admin
claimant does not begin with `<principal-name>#`.

## Policy layouts

Two provider sets ship with EntroQ:

- `conf/providers/entroq`: standalone OIDC identity projection plus example
  user/role permissions;
- `conf/providers/k8s`: Kubernetes service-account identity projection plus
  eqk8s mesh permissions.

Each deployment loads the core files and exactly one provider set. The provider
sets define the same Rego packages and cannot be loaded together.

For a standalone OIDC deployment:

```bash
opa run --server \
  /etc/opa/core \
  /etc/opa/providers/entroq \
  /etc/opa/local
```

`eqctl opa init <dir>` seeds core/provider policy and an editable user/role
document. Identity-provider settings belong to the EntroQ service flags, not
OPA data.

For Kubernetes, the Helm chart configures the service authenticator and loads
the k8s provider. The eqk8s operator continues to replace `data.mesh`
atomically as CRDs change; authentication caching never caches those decisions.

## Cache and rotation behavior

Successful JWT verification is cached by SHA-256 digest of the complete token.
The raw token is not stored in the cache key. Each entry expires at the earlier
of the configured token-cache TTL and the JWT's `exp`, and the LRU has a strict
entry bound. Invalid tokens and authorization decisions are never cached.

JWKS key material is cached separately. An unknown `kid` can trigger a refresh,
but refresh attempts are rate-limited to prevent attacker-selected key IDs from
turning authentication into an outbound-request amplifier. Concurrent refreshes
are coalesced. If the key source is unavailable after the key cache expires,
authentication fails closed with an unavailable error.

A removed signing key may remain accepted for at most the verified-token cache
TTL. Choose that TTL according to the environment's key-revocation requirement;
set it to zero when every call must be cryptographically reverified.

OPA policy and eqk8s CRD changes are not delayed by this cache because only
authentication facts are cached. Every operation still receives a fresh OPA
authorization decision.

## Errors

- Missing, malformed, expired, wrongly issued, wrongly targeted, or incorrectly
  signed credentials return gRPC `Unauthenticated`.
- JWKS source failures return `Unavailable`, allowing clients to distinguish an
  identity-provider outage from bad credentials.
- OPA denials return `PermissionDenied` with EntroQ authorization details.
- OPA transport or undefined-decision failures remain fail-closed.

## Migrating from EntroQ 1.8

This boundary is intentionally a minor-release breaking change:

1. Add `--authn=jwt` and the issuer/audience/JWKS service flags wherever
   `--authz=opahttp` is used.
2. Stop loading `core-entroq-jwt.rego`.
3. Remove JWKS, issuer, audience, and CA settings from OPA data.
4. Change custom `entroq.user` policy to read `input.principal.subject` or
   another field under `input.principal.claims`.
5. Custom Go service assembly must provide both
   `eqsvcgrpc.WithAuthenticator(...)` and `WithAuthorizer(...)`.
6. `authz.Request.Authz` and `authz.Authorization` no longer exist; authorization
   requests carry `Principal` instead.

The EntroQ protobuf and client bearer-token configuration are unchanged.

## Testing

Run authentication, policy, HTTP authorizer, and service-boundary tests with:

```bash
go test ./pkg/authn/... ./pkg/authz/... ./pkg/eqsvcgrpc ./cmd/internal/eqserve
```

Run the cached/native-verification microbenchmark with:

```bash
go test ./pkg/authn/jwtauthn -run '^$' -bench '^BenchmarkAuthenticate$' -benchmem
```
