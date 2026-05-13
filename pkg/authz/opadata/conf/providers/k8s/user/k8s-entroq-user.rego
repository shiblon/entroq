# Package entroq.user resolves a username from a Kubernetes service account JWT.
#
# The subject claim has the form "system:serviceaccount:<namespace>:<name>",
# which becomes the stable identity string used throughout the mesh model.
#
# Configure via OPA bundle data (data.json):
#   {
#     "entroq": {
#       "k8s": {
#         "jwks_url": "https://kubernetes.default.svc/openid/v1/jwks",
#         "audience": "https://kubernetes.default.svc.cluster.local",
#         "issuer":   "https://kubernetes.default.svc.cluster.local"
#       }
#     }
#   }
#
# Obtain cluster-specific values from:
#   kubectl get --raw /.well-known/openid-configuration
package entroq.user

import rego.v1

import data.entroq.jwt

name := jwt.verified_sub(
	input.authz.credentials,
	data.entroq.k8s.jwks_url,
	data.entroq.k8s.audience,
	data.entroq.k8s.issuer,
) if {
	input.authz.type == "Bearer"
}
