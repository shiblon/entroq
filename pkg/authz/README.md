# Authentication and authorization

EntroQ separates authentication from authorization:

- [`pkg/authn`](../authn) verifies request credentials and produces a trusted
  principal.
- `pkg/authz` describes the requested queue and namespace operations.
- [`pkg/authz/opahttp`](opahttp) sends the trusted principal and requested
  operations to Open Policy Agent (OPA).

The service never sends a bearer token to OPA. This keeps cryptographic JWT
validation in Go while leaving environment-specific identity projection and
access policy in Rego.

## Request flow

```text
Authorization: Bearer <JWT>
          │
          ▼
EntroQ Authenticator
  signature + issuer + audience + time checks
          │
          ▼
Verified Principal + requested operations
          │
          ▼
OPA Authorizer
          │
          ▼
allow / deny
```

`authn.VerifiedPrincipal` includes the verified subject, issuer, audiences, expiration,
and verified claims. It is constructed inside the service boundary and passed
explicitly to each authorization call. Clients cannot populate it through the
EntroQ protobuf or JSON API.

OPA receives input shaped like:

```json
{
  "principal": {
    "subject": "system:serviceaccount:payments:gateway",
    "issuer": "https://kubernetes.default.svc.cluster.local",
    "audience": ["https://kubernetes.default.svc.cluster.local"],
    "expires_at": 1788273063,
    "claims": {"sub": "system:serviceaccount:payments:gateway"}
  },
  "claimant_id": "system:serviceaccount:payments:gateway#sender",
  "queues": [{"exact": "/payments/worker/inbox", "actions": ["INSERT"]}]
}
```

The built-in `entroq.user` providers use `input.principal.subject` as the local
username and expose `input.principal.claims` for environment-specific mapping.
The `entroq.permissions` package remains the customization point for queue and
namespace grants.

See [OPA_AUTHZ.md](opadata/OPA_AUTHZ.md) for configuration, policy contracts,
cache behavior, and migration guidance.
