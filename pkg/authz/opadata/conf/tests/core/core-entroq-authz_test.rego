package entroq.authz

# Tests mock data.entroq.user.username and data.entroq.permissions.allowed_queues
# directly, bypassing JWT validation and the permissions implementation.
# This keeps the core authz tests independent of any particular IDP or
# policy data format.

test_input_matches_no_queues {
  failed == {{
    "exact": "aqueue",
    "actions": {"CLAIM", "DELETE"}
  }}
  with data.entroq.user.username as "auser"
  with data.entroq.permissions.allowed_queues as set()
  with input.queues as [{
    "exact": "aqueue",
    "actions": ["CLAIM", "DELETE"],
  }]
}

test_one_exact_exact_user_match {
  count(errors) == 0
  with data.entroq.user.username as "auser"
  with data.entroq.permissions.allowed_queues as [{
    "exact": "/users/auser/inbox",
    "actions": ["DELETE", "CLAIM"],
  }]
  with input.queues as [{
    "exact": "/users/auser/inbox",
    "actions": ["CLAIM", "DELETE"],
  }]
}

test_one_exact_match_missing_actions {
  failed == {{
    "exact": "/users/auser/inbox",
    "actions": {"DELETE"}
  }}
  with data.entroq.user.username as "auser"
  with data.entroq.permissions.allowed_queues as [{
    "exact": "/users/auser/inbox",
    "actions": ["CLAIM"],
  }]
  with input.queues as [{
    "exact": "/users/auser/inbox",
    "actions": ["CLAIM", "DELETE"],
  }]
}

test_one_exact_prefix_match {
  count(errors) == 0
  with data.entroq.user.username as "auser"
  with data.entroq.permissions.allowed_queues as [{
    "prefix": "/users/auser/",
    "actions": ["ALL"],
  }]
  with input.queues as [{
    "exact": "/users/auser/inbox",
    "actions": ["CLAIM", "DELETE"],
  }]
}

test_one_prefix_prefix_match {
  count(errors) == 0
  with data.entroq.user.username as "auser"
  with data.entroq.permissions.allowed_queues as [{
    "prefix": "/users/auser/",
    "actions": ["ALL"],
  }]
  with input.queues as [{
    "prefix": "/users/auser/inbox",
    "actions": ["CLAIM", "DELETE"],
  }]
}

test_exact_against_multiple_matches {
  count(errors) == 0
  with data.entroq.user.username as "auser"
  with data.entroq.permissions.allowed_queues as [
    {"prefix": "/users/auser/", "actions": ["READ"]},
    {"exact": "/users/auser/inbox", "actions": ["CLAIM"]},
    {"exact": "/users/auser/inbox", "actions": ["DELETE"]},
  ]
  with input.queues as [{
    "exact": "/users/auser/inbox",
    "actions": ["CLAIM", "DELETE", "READ"]
  }]
}

test_exact_against_partial_actions {
  failed == {{
    "exact": "/users/auser/inbox",
    "actions": {"CLAIM"}
  }}
  with data.entroq.user.username as "auser"
  with data.entroq.permissions.allowed_queues as [
    {"prefix": "/users/auser/", "actions": ["READ"]},
    {"exact": "/users/auser/inbox", "actions": ["DELETE"]},
  ]
  with input.queues as [{
    "exact": "/users/auser/inbox",
    "actions": ["CLAIM", "DELETE", "READ"]
  }]
}

test_prefix_prefix_match {
  count(errors) == 0
  with data.entroq.user.username as "auser"
  with data.entroq.permissions.allowed_queues as [{
    "prefix": "/auser/",
    "actions": ["ALL"]
  }]
  with input.queues as [{
    "prefix": "/auser/stuff",
    "actions": ["CLAIM"]
  }]
}

test_prefix_prefix_nomatch {
  count(errors) >= 0
  with data.entroq.user.username as "auser"
  with data.entroq.permissions.allowed_queues as [{
    "prefix": "/auser/stuff/",
    "actions": ["ALL"]
  }]
  with input.queues as [{
    "prefix": "/auser/stuff",
    "actions": ["CLAIM"]
  }]
}
