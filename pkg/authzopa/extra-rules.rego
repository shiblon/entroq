package entroq.authz

# TODO: show how token parsing can work.
#username = u {
#  u := input.authz["token"]
#  u != ""
#}

# Add a special /ns=user/<username> queue prefix to everyone, that they can do what they want with.
user_queues[q] {
  q := {
    "prefix": concat("", ["/ns=user/", username, "/"]),
    "actions": ["ALL"]
  }
}

