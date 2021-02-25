package entroq.permissions

import data.entroq.user as equser

# Here we're using the policy package as "user and role data".
# There are other ways of doing it, like having actual data pushed into the
# service as a document, and then relying on that. This approach allows the
# data to be specified in Rego. See the example "policy" implementation outside
# of this config tree (outside to allow testing without relying on data from a
# file).
import data.entroq.policy

# The user with this username. Rego will error out if there is more than one.
this_user = u {
  u := policy.users[_]
  u.name == equser.username
}

user_queues[qs] {
  qs := this_user.queues[_]
}

# Add a special /ns=user/<username> queue prefix to everyone, that they can do what they want with.
user_queues[q] {
  q := {
    "prefix": concat("", ["/ns=user/", equser.username, "/"]),
    "actions": ["ALL"]
  }
}

role_queues[r.queues[_]] {
  r := policy.roles[_]
  ({x | x := this_user.roles[_]} | {"*"})[r.name]
}

allowed_queues[q] {
  q := (user_queues | role_queues)[_]
}
