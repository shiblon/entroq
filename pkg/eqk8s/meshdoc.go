// Package eqk8s provides types and utilities for the EntroQ k8s mesh operator.
package eqk8s

import "github.com/shiblon/entroq/pkg/authz/meshpolicy"

// MeshDocument is the complete authorization document produced by the
// operator.
type MeshDocument = meshpolicy.Document

// QueuePolicy describes queue access derived from an EntroQQueue resource.
type QueuePolicy = meshpolicy.QueuePolicy

// NamespacePolicy describes document namespace access derived from an
// EntroQQueue resource.
type NamespacePolicy = meshpolicy.NamespacePolicy

// Identity maps an authenticated subject to operator-asserted labels.
type Identity = meshpolicy.Identity

// OPADocument is the top-level document used by OPA's data API.
type OPADocument struct {
	Mesh OPAMesh `json:"mesh"`
}

// OPAMesh preserves the API used by external OPA integrations.
// Deprecated: use MeshDocument.
type OPAMesh = meshpolicy.Document

// OPAQueuePolicy preserves the API used by external OPA integrations.
// Deprecated: use QueuePolicy.
type OPAQueuePolicy = meshpolicy.QueuePolicy

// OPANamespacePolicy preserves the API used by external OPA integrations.
// Deprecated: use NamespacePolicy.
type OPANamespacePolicy = meshpolicy.NamespacePolicy

// OPAIdentity preserves the API used by external OPA integrations.
// Deprecated: use Identity.
type OPAIdentity = meshpolicy.Identity
