// Package eqk8s provides types and utilities for the EntroQ k8s mesh operator.
package eqk8s

// OPADocument is the top-level document pushed to OPA's data API by the operator.
// It is accessible in Rego as data.mesh.
type OPADocument struct {
	Mesh OPAMesh `json:"mesh"`
}

// OPAMesh holds the two sections of mesh authorization data.
type OPAMesh struct {
	// Initialized is always true when written by the operator. Rego checks this
	// to distinguish "document not yet loaded" from "no policy exists".
	Initialized bool `json:"initialized"`

	// Queues is the list of queue policies derived from EntroQQueue resources.
	Queues []OPAQueuePolicy `json:"queues"`

	// Identities maps each service account identity string to its mesh label
	// claims, derived from EntroQIdentity resources.
	// Keys are of the form "system:serviceaccount:<namespace>:<name>".
	Identities map[string]OPAIdentity `json:"identities"`
}

// OPAQueuePolicy describes a single queue pattern and the callers permitted to
// access it.
type OPAQueuePolicy struct {
	// Pattern is the queue path or pattern.
	Pattern string `json:"pattern"`

	// MatchType is one of "Exact", "Prefix", or "Glob".
	MatchType string `json:"matchType"`

	// AllowedCallers is a list of label requirement sets. A caller is permitted
	// if its labels satisfy at least one entry (OR across entries, AND within).
	AllowedCallers []map[string]string `json:"allowedCallers"`
}

// OPAIdentity holds the mesh label claims asserted for a service account.
type OPAIdentity struct {
	Labels map[string]string `json:"labels"`
}
