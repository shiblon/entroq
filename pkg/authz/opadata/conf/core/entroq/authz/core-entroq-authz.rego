# The core rules in this file are _always_ present in the system.
# This file deals with the input, which is fixed-format and determined
# by the needs of the EntroQ system itself.
#
# The data, on the other hand, can come from various places, optionally.
# It is possible for example, to bring information about users, roles, and
# privileges in from another server entirely, all within Rego code. We enable
# this flexibility by keeping the data-specific processing separate from the
# non-optional user-request processing.
#
# A working implementation comes from importing two things from a package named
# entroq.permissions:
#
# - username: a complete rule producing a string containing the name of the
#       user (a user ID).
# - allowed_queues: a partial rule producing all allowed queue specifications
#       for this user ID.
#
# How these are obtained is up to the deployer of the service. A default
# configuration is given in default-permissions.rego. Comments there indicate
# the shape of the data that it works with.
package entroq.authz

import rego.v1

import data.entroq.namespaces
import data.entroq.permissions
import data.entroq.queues
import data.entroq.user

failed contains q if {
	some q in queues.disallowed(input.queues, permissions.allowed_queues)
}

failed_namespaces contains n if {
	some n in namespaces.disallowed(input.namespaces, permissions.allowed_namespaces)
}

# claimant_mismatch is true when the caller supplies a claimant_id that does
# not begin with their authenticated identity (user.name + "#"). This prevents
# cross-identity impersonation while allowing per-process nonces within the
# same identity. Only fires when both sides are present; unauthenticated
# requests and empty claimant_ids pass through unchanged.
#
# To allow admins to operate under any claimant (e.g. for forced deletion),
# add an exception in your permissions policy:
#
#   import data.entroq.authz
#   claimant_mismatch := false if { data.entroq.permissions.is_admin }
claimant_mismatch if {
	input.claimant_id != ""
	user.name
	not startswith(input.claimant_id, concat("", [user.name, "#"]))
	not permissions.is_admin
}

# Add a message containing user information if there are queue or namespace mismatches.
errors contains msg if {
	count(failed) + count(failed_namespaces) > 0
	user.name
	msg := concat("", ["User: ", user.name])
}

errors contains "claimant_id does not match authenticated user" if {
	claimant_mismatch
}

default allow := false

allow if {
	# It is possible to have allowed resources for non-authorized users.
	# We only say "allow" if there are, in fact, some resources that _could_ be
	# allowed.
	count(permissions.allowed_queues) + count(permissions.allowed_namespaces) > 0

	# Only allow if none of the allowed queues failed.
	count(failed) == 0

	# Only allow if none of the allowed namespaces failed.
	count(failed_namespaces) == 0

	# Only allow if there are no additional errors.
	count(errors) == 0
}
