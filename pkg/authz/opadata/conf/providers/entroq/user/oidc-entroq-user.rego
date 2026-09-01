# Package entroq.user resolves a username from the principal authenticated by
# the EntroQ service.
#
# Signature, issuer, audience, expiry, and not-before checks happen before this
# policy is called. Customize this small projection when an environment uses a
# verified claim other than sub as its local username.
package entroq.user

import rego.v1

name := input.principal.subject

# Expose verified custom claims to environment policy without exposing the JWT.
claims := input.principal.claims
