package entroq.authz

test_actions_left {
  actions_left({"CLAIM"}, {"READ"}) == {"CLAIM"}
  actions_left({"CLAIM"}, {"ALL"}) == set()
  actions_left({"ALL"}, {"CLAIM"}) == {"ALL"} # Should never happen - user never requests "ALL".
  actions_left({"CLAIM"}, {"CLAIM", "DEPEND", "READ"}) == set()
}

test_role_auto_only {
  # Make sure the wildcard is always in the role names
  user_role_names["*"]
  with username as "blah"
  with data.roles as []
  with data.users as []
}

test_role_auto_added {
  user_role_names["*"]
  with username as "blah"
  with data.users as [{
    "name": "blah",
    "roles": ["role1", "role2"]
  }]
}

test_role_auto_with_others {
  count(user_role_names) == 3
  with username as "blah"
  with data.users as [{
    "name": "blah",
    "roles": ["role1", "role2"]
  }]
}

test_role_auto_with_no_user {
  count(user_role_names) == 1
  with username as "blah"
  with data.users as []
}

test_no_user_queues {
  count(user_queues) == 0
  with username as "blah"
  with data.users as []
}

test_user_with_queues {
  count(user_queues) == 2
  with username as "blah"
  with data.users as [{
    "name": "blah",
    "queues": [{
      "exact": "q1",
      "actions": ["READ"]
    },
    {
      "exact": "q2",
      "actions": ["CLAIM"]
    }]
  }]
}

test_no_matching_user_queues {
  count(user_queues) == 0
  with username as "auser"
  with input.queues as [{
    "exact": "aqueue",
    "actions": ["CLAIM"]
  }]
  with data.users as []
  with data.roles as [{
    "name": "*",
    "queues": [{
      "prefix": "/ns=global/",
      "actions": ["READ"],
    }]
  }]
}

test_only_match_wildcard_role_queues {
  count(role_queues) == 2
  with username as "blah"
  with input.queues as [{
    "exact": "/ns=global/inbox",
    "actions": ["CLAIM"]
  }]
  with data.users as []
  with data.roles as [{
    "name": "*",
    "queues": [{
      "prefix": "/ns=global/",
      "actions": ["READ"],
    },
    {
      "exact": "/ns=global/inbox",
      "actions": ["CLAIM"],
    }]
  }]
}

test_only_match_wildcard_role_queues_single {
  count(role_queues) == 1
  with username as "auser"
  with input.queues as [{
    "exact": "aqueue",
    "actions": ["CLAIM"]
  }]
  with data.users as []
  with data.roles as [{
    "name": "*",
    "queues": [{
      "prefix": "/ns=global/",
      "actions": ["READ"],
    }]
  }]
}

test_input_matches_no_queues {
  failed_queues == {{
    "exact": "aqueue",
    "actions": {"CLAIM", "DELETE"}
  }}
  with username as "auser"
  with input.queues as [{
    "exact": "aqueue",
    "actions": ["CLAIM", "DELETE"],
  }]
  with data.users as []
  with data.roles as []
}

test_one_exact_exact_user_match {
  allow
  with username as "auser"
  with input.queues as [{
    "exact": "/users/auser/inbox",
    "actions": ["CLAIM", "DELETE"],
  }]
  with data.users as [{
    "name": "auser",
    "queues": [{
      "exact": "/users/auser/inbox",
      "actions": ["DELETE", "CLAIM"],
    }]
  }]
}

test_one_exact_match_missing_actions {
  failed_queues == {{
    "exact": "/users/auser/inbox",
    "actions": {"DELETE"}
  }}
  with username as "auser"
  with input.queues as [{
    "exact": "/users/auser/inbox",
    "actions": ["CLAIM", "DELETE"],
  }]
  with data.users as [{
    "name": "auser",
    "queues": [{
      "exact": "/users/auser/inbox",
      "actions": ["CLAIM"],
    }]
  }]
}

test_one_exact_prefix_match {
  allow
  with username as "auser"
  with input.queues as [{
    "exact": "/users/auser/inbox",
    "actions": ["CLAIM", "DELETE"],
  }]
  with data.users as [{
    "name": "auser",
    "queues": [{
      "prefix": "/users/auser/",
      "actions": ["ALL"],
    }]
  }]
}

test_one_prefix_prefix_match {
  allow
  with username as "auser"
  with input.queues as [{
    "prefix": "/users/auser/inbox",
    "actions": ["CLAIM", "DELETE"],
  }]
  with data.users as [{
    "name": "auser",
    "queues": [{
      "prefix": "/users/auser/",
      "actions": ["ALL"],
    }]
  }]
}

test_exact_against_multiple_mathches {
  allow
  with username as "auser"
  with input.queues as [{
    "exact": "/users/auser/inbox",
    "actions": ["CLAIM", "DELETE", "READ"]
  }]
  with data.users as [{
    "name": "auser",
    "roles": ["auser"],
    "queues": [{
      "prefix": "/users/auser/",
      "actions": ["READ"]
    },
    {
      "exact": "/users/auser/inbox",
      "actions": ["CLAIM"]
    }]
  }]
  with data.roles as [{
    "name": "auser",
    "queues": [{
      "exact": "/users/auser/inbox",
      "actions": ["DELETE"],
    }]
  }]
}

test_exact_against_partial_actions {
  failed_queues == {{
    "exact": "/users/auser/inbox",
    "actions": {"CLAIM"}
  }}
  with username as "auser"
  with input.queues as [{
    "exact": "/users/auser/inbox",
    "actions": ["CLAIM", "DELETE", "READ"]
  }]
  with data.users as [{
    "name": "auser",
    "roles": ["auser"],
    "queues": [{
      "prefix": "/users/auser/",
      "actions": ["READ"]
    }]
  }]
  with data.roles as [{
    "name": "auser",
    "queues": [{
      "exact": "/users/auser/inbox",
      "actions": ["DELETE"],
    }]
  }]
}

test_prefix_prefix_match {
  allow
  with username as "auser"
  with input.queues as [{
    "prefix": "/auser/stuff",
    "actions": ["CLAIM"]
  }]
  with data.users as [{
    "name": "auser",
    "queues": [{
      "prefix": "/auser/",
      "actions": ["ALL"]
    }]
  }]
}

test_prefix_prefix_nomatch {
  not allow
  with username as "auser"
  with input.queues as [{
    "prefix": "/auser/stuff",
    "actions": ["CLAIM"]
  }]
  with data.users as [{
    "name": "auser",
    "queues": [{
      "prefix": "/auser/stuff/",
      "actions": ["ALL"]
    }]
  }]
}
