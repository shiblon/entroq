# Package entroq.user resolves a username from the incoming request.
#
# This is the built-in OIDC provider. It works with any IDP that serves a
# standard JWKS endpoint: Google, Auth0, Okta, Azure AD, Keycloak, and others.
#
# It expects:
#   input.authz.type        = "Bearer"
#   input.authz.credentials = <JWT from the Authorization header>
#
# Configure via OPA bundle data (data.json):
#   {
#     "entroq": {
#       "idp": {
#         "jwks_url": "https://www.googleapis.com/oauth2/v3/certs",
#         "audience": "your-client-id",
#         "issuer":   "https://accounts.google.com"
#       }
#     }
#   }
#
# See OPA_AUTHZ.md for IDP-specific values.
package entroq.user

import rego.v1

# Fetch the JWKS from the IDP. OPA caches this response automatically.
jwks_response := http.send({
	"method": "GET",
	"url": data.entroq.idp.jwks_url,
	"cache": true,
	"tls_use_system_certs": true,
})

# Verify the JWT and extract the subject claim as the username.
# This rule is undefined (fails closed) if:
#   - the Authorization type is not "Bearer"
#   - the token signature is invalid
#   - the audience or issuer does not match
#   - the token is expired
name := u if {
	input.authz.type == "Bearer"
	token := input.authz.credentials
	[valid, _, payload] := io.jwt.decode_verify(token, {
		"cert": json.marshal(jwks_response.body),
		"aud": data.entroq.idp.audience,
		"iss": data.entroq.idp.issuer,
	})
	valid
	u := payload.sub
}
