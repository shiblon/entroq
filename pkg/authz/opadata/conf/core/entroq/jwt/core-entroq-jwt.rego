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
# The JWKS response is cached by OPA (respecting HTTP cache headers), so the
# network fetch only occurs on startup and after cache expiry. The print()
# below fires once per cache miss -- silent in normal operation, visible on
# failure so operators can diagnose JWKS access problems in pod logs.
#
# NOTE: the k8s OIDC JWKS endpoint requires anonymous read access. On Minikube
# this is not enabled by default. Fix:
#   kubectl create clusterrolebinding oidc-discovery-anonymous \
#     --clusterrole=system:service-account-issuer-discovery \
#     --group=system:unauthenticated
package entroq.jwt

import rego.v1

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
	print("entroq.jwt: JWKS fetch from", config.jwks_url, "->", resp.status_code)
	resp.status_code == 200
	[valid, _, payload] := io.jwt.decode_verify(token, {
		"cert": json.marshal(resp.body),
		"aud": config.audience,
		"iss": config.issuer,
	})
	valid
	sub := payload.sub
}
