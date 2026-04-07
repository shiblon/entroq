# Package entroq.user resolves a username from the incoming request.
#
# This is the EXAMPLE variant that reads JWKS key material inline from OPA
# bundle data (data.entroq.idp.jwks), rather than fetching from a live URL.
# This avoids needing a running IDP for the example.
#
# For production, replace this file with the reference implementation at:
#   pkg/authz/opadata/conf/example/example-entroq-user.rego
#
# That version fetches the JWKS from data.entroq.idp.jwks_url at evaluation
# time (with OPA's built-in caching), which is the correct approach for a
# real IDP such as Google, Okta, Auth0, or Keycloak.
#
# The data.entroq.idp fields used here:
#   jwks     - JSON string of the JWKS document (inline public key material)
#   audience - expected "aud" claim in the JWT
#   issuer   - expected "iss" claim in the JWT
package entroq.user

import rego.v1

# Verify the JWT and extract the subject claim as the username.
# This rule is undefined (fails closed) if:
#   - the Authorization type is not "Bearer"
#   - the token signature is invalid against the inline JWKS
#   - the audience or issuer does not match
#   - the token is expired
username := u if {
	input.authz.type == "Bearer"
	token := input.authz.credentials
	[valid, _, payload] := io.jwt.decode_verify(token, {
		"cert": data.entroq.idp.jwks,
		"aud":  data.entroq.idp.audience,
		"iss":  data.entroq.idp.issuer,
	})
	valid
	u := payload.sub
}
