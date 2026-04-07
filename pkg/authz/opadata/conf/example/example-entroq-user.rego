# Package entroq.user resolves a username from the incoming request.
#
# This is a REFERENCE IMPLEMENTATION. Copy and adapt it for your IDP.
#
# It expects:
#   input.authz.type        = "Bearer"
#   input.authz.credentials = <JWT from the Authorization header>
#
# And it reads JWKS settings from OPA bundle data:
#   data.entroq.idp.jwks_url  - the URL to fetch public keys from
#   data.entroq.idp.audience  - the expected "aud" claim in the JWT
#   data.entroq.idp.issuer    - the expected "iss" claim in the JWT
#
# Example OPA bundle data (data.json):
#   {
#     "entroq": {
#       "idp": {
#         "jwks_url": "https://www.googleapis.com/oauth2/v3/certs",
#         "audience": "your-google-client-id",
#         "issuer":   "https://accounts.google.com"
#       }
#     }
#   }
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
username := u if {
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
