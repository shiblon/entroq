# Package entroq.user resolves a username from the principal authenticated by
# the EntroQ service.
#
# The subject claim has the form "system:serviceaccount:<namespace>:<name>",
# which becomes the stable identity string used throughout the mesh model.
#
# Signature, issuer, audience, expiry, and not-before checks happen before this
# policy is called. input.principal and its claims are server-constructed
# verified facts; the bearer token is never sent to OPA.
package entroq.user

import rego.v1

name := input.principal.subject

# Expose verified custom claims to environment policy without exposing the JWT.
claims := input.principal.claims
