# Package entroq.jwt provides shared JWT verification for entroq.user providers.
#
# verified_sub accepts a raw token and a config object with the following fields:
#
#   jwks_url     (required) - URL of the JWKS endpoint
#   audience     (required) - expected audience claim
#   issuer       (required) - expected issuer claim
#   ca_cert_file (optional) - path to a CA certificate file for TLS verification;
#                             useful for in-cluster k8s API server endpoints that
#                             use a self-signed cluster CA (e.g.
#                             /var/run/secrets/kubernetes.io/serviceaccount/ca.crt)
#
# NOTE: the k8s OIDC JWKS endpoint requires anonymous read access. On Minikube
# this is not enabled by default. Fix:
#   kubectl create clusterrolebinding oidc-discovery-anonymous \
#     --clusterrole=system:service-account-issuer-discovery \
#     --group=system:unauthenticated
package entroq.jwt

import rego.v1

# _log_jwks logs a warning when the JWKS fetch returns a non-200 status.
# The two branches together always succeed regardless of status, so this
# never fails the calling rule. print() fires only on the error branch.
_log_jwks(url, status) if {
	status != 200
	print("entroq.jwt: JWKS fetch from", url, "returned HTTP", status,
		"-- JWT verification will fail.",
		"On Minikube: kubectl create clusterrolebinding oidc-discovery-anonymous",
		"--clusterrole=system:service-account-issuer-discovery --group=system:unauthenticated")
}

_log_jwks(_, status) if {
	status == 200
}

verified_sub(token, config) := sub if {
	ca_cert := object.get(config, "ca_cert_file", "")
	ca_opts := {"tls_ca_cert_file": v | v := ca_cert; ca_cert != ""}
	resp := http.send(object.union(
		{
			"method": "GET",
			"url": config.jwks_url,
			"cache": true,
			"tls_use_system_certs": true,
		},
		ca_opts,
	))
	_log_jwks(config.jwks_url, resp.status_code)
	resp.status_code == 200
	[valid, _, payload] := io.jwt.decode_verify(token, {
		"cert": json.marshal(resp.body),
		"aud": config.audience,
		"iss": config.issuer,
	})
	valid
	sub := payload.sub
}
