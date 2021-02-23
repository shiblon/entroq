package entroq.queues

# This package operates on queues and sets of queue specs.
# Queue specs are expected to look like this:
# {
#   "exact": "/exact/name/of/queue",
#   "actions": ["CLAIM", "READ"],
# }
#
# Instead of "exact", a queue may have "prefix". Actions are strings. Currently allowed are
# - READ
# - CHANGE
# - CLAIM
# - INSERT
# - DELETE
# - ALL
#
# Though the only special action above is "ALL", and it is treated as though it contains
# all of the actions that can be had (allows anything to match it).

# name_match returns true if "want" is match by "can" in a way that would
# permit this queue to be considered for use by a user requesting it.
#
# - want: a queue spec representing a queue (or queue pattern) the user wishes to use
# - can: a queue spec representing a queue or pattern the data allows.
#
# Use this to find out which "allowed" queue specs pertain to a given user queue request.
name_match(want, can) {
  want.exact == can.exact
}
name_match(want, can) {
  startswith(want.exact, can.prefix)
}
name_match(want, can) {
  startswith(want.prefix, can.prefix)
}

# actions_left returns actions in "want" that are not covered by actions in "can".
#
# Use this on the actions of a particular user queue request and a matching
# queue allowance from the data. Best when used only after a positive name_match
#
# If "ALL" is in "can", always returns the empty set (all are allowed, none are left).
#
# - want: a set of action strings that the user wishes to perform.
# - can: a set of action strings that might be allowed.
actions_left(want, can) = x {
  x := {y | y := (want - can)[_]; not can["ALL"]}
}

# disallowed returns a set of queue specs, with actions filled in
# that are not covered by any of the given allowances.
#
# - want: a set of queue specs that the user wants to authorize.
# - can: a set of queue specs that are allowed for this user.
disallowed(want, can) = results {
  results := {q |
    want_q := want[_]
    want_actions := {a | a := want_q.actions[_]}
    can_actions := {aq.actions[_] | aq := can[_]; name_match(want_q, aq)}

    left := actions_left(want_actions, can_actions)
    count(left) > 0

    q := object.union(
      object.remove(want_q, ["actions"]), {"actions": left}
    )
  }
}
