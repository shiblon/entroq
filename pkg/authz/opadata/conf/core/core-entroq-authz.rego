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

import data.entroq.queues
import data.entroq.user.username
import data.entroq.permissions.allowed_queues

failed[q] {
  q := queues.disallowed(input.queues, allowed_queues)[_]
}

# Add a message containing user information if there are queue mismatches.
errors[msg] {
  count(failed) > 0
  username
  msg := concat("User: ", username)
}

default allow = false
allow {
  # It is possible to have allowed queues for non-authorized users.
  # We only say "allow" if there are, in fact, some queues that _could_ be
  # allowed.
  count(allowed_queues) > 0

  # Only allow if none of the allowed queues failed.
  count(failed) == 0

  # Only allow if there are no additional errors.
  count(errors) == 0
}

