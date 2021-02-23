package entroq.authz

test_input_matches_no_queues {
  failed_queues == {{
    "exact": "aqueue",
    "actions": {"CLAIM", "DELETE"}
  }}
  with input.authz as {"testuser": "auser"}
  with input.queues as [{
    "exact": "aqueue",
    "actions": ["CLAIM", "DELETE"],
  }]
  with data.users as []
  with data.roles as []
}

test_one_exact_exact_user_match {
  allow
  with input.authz as {"testuser": "auser"}
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
  with input.authz as {"testuser": "auser"}
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
  with input.authz as {"testuser": "auser"}
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
  with input.authz as {"testuser": "auser"}
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
  with input.authz as {"testuser": "auser"}
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
  with input.authz as {"testuser": "auser"}
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
  with input.authz as {"testuser": "auser"}
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
  with input.authz as {"testuser": "auser"}
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
