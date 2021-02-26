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

import data.entroq.permissions
import data.entroq.queues
import data.entroq.user.username

failed[q] {
  q := queues.disallowed(input.queues, permissions.allowed_queues)[_]
}

# Errors are not merely helpful, they are essential. The presence of an error
# can signal a lack of authorization *even if there are no computable failed
# queues*, which can happen in several circumstances (like a username not
# present in the query).
errors[msg] {
  not username
  msg := "No username found"
}
errors[msg] {
  username == ""
  msg := "Empty username found"
}
# Add a message containing user information if there are queue mismatches.
errors[msg] {
  count(failed) > 0
  username
  msg := concat("User: ", username)
}

default allow = false
allow {
  username
  not failed
  not errors
}

