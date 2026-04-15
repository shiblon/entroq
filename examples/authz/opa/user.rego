# Package entroq.user resolves the caller's name from the incoming request.
#
# This is the EXAMPLE variant that reads JWKS key material inline from OPA
# bundle data (data.entroq.idp.jwks), rather than fetching from a live URL.
# This avoids needing a running IDP for the example.
#
# For production with a real IDP, remove this file entirely. The built-in
# provider at conf/providers/entroq/user/oidc-entroq-user.rego fetches the
# JWKS from data.entroq.idp.jwks_url at evaluation time (with OPA's built-in
# caching). Just add jwks_url to data.json and drop this file.
#
# The data.entroq.idp fields used here (inline-key variant for local dev):
#   jwks     - JSON string of the JWKS document (inline public key material)
#   audience - expected "aud" claim in the JWT
#   issuer   - expected "iss" claim in the JWT
package entroq.user

import rego.v1

# Verify the JWT and extract the subject claim as the caller's name.
# This rule is undefined (fails closed) if:
#   - the Authorization type is not "Bearer"
#   - the token signature is invalid against the inline JWKS
#   - the audience or issuer does not match
#   - the token is expired
name := u if {
	input.authz.type == "Bearer"
	token := input.authz.credentials

	[valid, _, payload] := io.jwt.decode_verify(token, {
		"cert": data.entroq.idp.jwks,
		"aud": data.entroq.idp.audience,
		"iss": data.entroq.idp.issuer,
	})
	valid
	u := payload.sub
}
