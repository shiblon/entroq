package entroq.authz

import data.entroq.queues

username = u {
  u := input.authz.testuser
  u != ""
}

# The user with this username. Rego will error out if there is more than one.
this_user = u {
  u := data.users[_]
  u.name == username
}

user_queues[qs] {
  qs := this_user.queues[_]
}

role_queues[r.queues[_]] {
  r := data.roles[_]
  ({x | x := this_user.roles[_]} | {"*"})[r.name]
}

failed_queues[q] {
  q := queues.disallowed(input.queues, role_queues | user_queues)[_]
}

allow {
  count(failed_queues) == 0
}
