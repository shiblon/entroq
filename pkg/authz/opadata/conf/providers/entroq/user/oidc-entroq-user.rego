# Package entroq.user resolves a username from an OIDC JWT.
#
# Works with any IDP that serves a standard JWKS endpoint: Google, Auth0,
# Okta, Azure AD, Keycloak, and others.
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

import data.entroq.jwt

name := jwt.verified_sub(
	input.authz.credentials,
	data.entroq.idp.jwks_url,
	data.entroq.idp.audience,
	data.entroq.idp.issuer,
) if {
	input.authz.type == "Bearer"
}
