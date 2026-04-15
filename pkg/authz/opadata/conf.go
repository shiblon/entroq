package authz

import "embed"

// ConfFS contains the embedded OPA policy files for EntroQ authorization.
// It includes the core logic (conf/core/) and the built-in OIDC provider
// (conf/providers/). Test files are not included.
//
// Use this to seed a local OPA configuration directory via eqctl opa init.
//
//go:embed conf/core conf/providers
var ConfFS embed.FS
