package entroq.authz

username = u {
  u := input.authz.testuser
  u != ""
}

# The user with this username. Rego will error out if there is more than one.
this_user = u {
  u := data.users[_]
  u.name == username
}

# Roles that can be used to allow this user.
user_role_names[rn] {
  rn := "*"
}

user_role_names[rn] {
  is_object(this_user)
  rn := this_user.roles[_]
}

user_queues[qs] {
  is_object(this_user)
  qs := this_user.queues[_]
}

role_queues[qs] {
  r := data.roles[_]
  qs := r.queues[_]
  user_role_names[r.name]
}

possible_queues := role_queues | user_queues

name_matches(want, can) {
  want.exact == can.exact
}
name_matches(want, can) {
  startswith(want.exact, can.prefix)
}
name_matches(want, can) {
  startswith(want.prefix, can.prefix)
}

# Find out what actions are not covered by allowed listings.
actions_left(want, can) = x {
  x := {y | y := (want - can)[_]; not can["ALL"]}
}

failed_queues[q] {
  my_q := input.queues[_]
  want := {x | x := my_q.actions[_]}
  # What we are allowed to do goes into "can".
  # Determined by the union of all allowed actions across
  # any queue spec that matches this one.
  can := {aq.actions[_] |
    aq := possible_queues[_]
    name_matches(my_q, aq)
  }
  # Replace the actions wanted in the input object with
  # actions not satisfied by the allowed actions for any matching queues.
  # Do this by removing actions and replacing them using an object union.
  q := object.union(object.remove(my_q, ["actions"]), {"actions": actions_left(want, can)})

  # Only return queues that have missing actions. If there are none of these, the user is allowed.
  count(q.actions) > 0
}

allow {
  count(failed_queues) == 0
}

# Example of how to add username-based queues even thought they're implicit.
# This could be imported from another module if we want to keep "standard"
# rules separate from system-dependent rules. You can specify as many of these
# as you like.
#
#user_queues[q] {
#  q := {
#    "prefix": concat("", ["/ns=user/", username, "/"]),
#    "actions": ["ALL"]
#  }
#}
