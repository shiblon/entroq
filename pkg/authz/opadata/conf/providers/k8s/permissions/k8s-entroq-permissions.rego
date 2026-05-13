# Package entroq.permissions derives queue access grants from the EntroQ mesh
# document maintained by the eqk8s operator.
#
# Two grant sources:
#
# 1. Auto-grant: every service account gets ALL on its own queue prefix.
#    "system:serviceaccount:payments:svc-a" -> ALL on "/payments/svc-a/".
#    This covers the service's own inbox, response queues, and GC prefix
#    without any operator configuration.
#
# 2. Mesh grant: the operator pushes data.mesh from EntroQQueue and
#    EntroQIdentity CRDs. If the caller's label claims satisfy a queue
#    policy's allowedCallers predicate, they are granted ALL on that queue.
#    Label matching: AND within one allowedCallers entry, OR across entries.
#
# If data.mesh.initialized is false or absent (OPA just restarted, operator
# hasn't reconciled yet), mesh grants are suppressed. The auto-grant still
# fires, so a service can always reach its own queues. Cross-service calls
# are denied until the mesh document is present.
package entroq.permissions

import rego.v1

import data.entroq.user as equser

# own_prefix is "/ns/name/" derived from "system:serviceaccount:ns:name".
# Undefined for any subject that does not match the k8s SA format.
own_prefix := prefix if {
	parts := split(equser.name, ":")
	count(parts) == 4
	parts[0] == "system"
	parts[1] == "serviceaccount"
	prefix := concat("/", ["", parts[2], parts[3], ""])
}

allowed_queues contains {"prefix": own_prefix, "actions": ["ALL"]} if {
	own_prefix
}

# identity is the mesh record for this caller, if present.
identity := data.mesh.identities[equser.name]

# Mesh grants: one entry per queue policy the caller satisfies.
allowed_queues contains q if {
	data.mesh.initialized
	identity
	some policy in data.mesh.queues
	caller_satisfies(identity.labels, policy.allowedCallers)
	q := queue_spec(policy)
}

# queue_spec translates a mesh policy pattern into an entroq.queues-compatible
# spec object. Exact patterns use "exact"; Prefix patterns use "prefix".
queue_spec(policy) := {"exact": policy.pattern, "actions": ["ALL"]} if {
	policy.matchType == "Exact"
}

queue_spec(policy) := {"prefix": policy.pattern, "actions": ["ALL"]} if {
	policy.matchType == "Prefix"
}

# caller_satisfies: true if the caller's labels satisfy at least one matcher (OR).
caller_satisfies(labels, matchers) if {
	some matcher in matchers
	every key, val in matcher {
		labels[key] == val
	}
}

allowed_namespaces := set()
