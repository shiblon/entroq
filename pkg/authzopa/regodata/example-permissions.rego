package entroq.permissions

import data.entroq.user.username

# The user with this username. Rego will error out if there is more than one.
this_user = u {
  u := data.users[_]
  u.name == username
}

user_queues[qs] {
  qs := this_user.queues[_]
}

# Add a special /ns=user/<username> queue prefix to everyone, that they can do what they want with.
user_queues[q] {
  q := {
    "prefix": concat("", ["/ns=user/", username, "/"]),
    "actions": ["ALL"]
  }
}

role_queues[r.queues[_]] {
  r := data.roles[_]
  ({x | x := this_user.roles[_]} | {"*"})[r.name]
}

allowed_queues[q] {
  q := (user_queues | role_queues)[_]
}
