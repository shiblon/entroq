# Package entroq.jwt provides shared JWT verification for entroq.user providers.
#
# User providers call verified_sub with the raw token and their IdP config
# values. The function fetches the JWKS, verifies the token, and returns the
# subject claim. OPA caches the JWKS HTTP response by URL at the engine level,
# so moving the fetch into a function does not cause extra network round-trips.
package entroq.jwt

import rego.v1

# verified_sub verifies a Bearer JWT and returns its subject claim.
# Undefined (fails closed) when the token is invalid, expired, or the
# audience/issuer do not match.
verified_sub(token, jwks_url, audience, issuer) := sub if {
	resp := http.send({
		"method": "GET",
		"url": jwks_url,
		"cache": true,
		"tls_use_system_certs": true,
	})
	[valid, _, payload] := io.jwt.decode_verify(token, {
		"cert": json.marshal(resp.body),
		"aud": audience,
		"iss": issuer,
	})
	valid
	sub := payload.sub
}
