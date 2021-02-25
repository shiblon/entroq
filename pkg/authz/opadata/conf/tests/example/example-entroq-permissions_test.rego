package entroq.permissions

test_this_user_found {
  this_user == {
    "name": "auser",
    "queues": [{
      "exact": "/a/queue",
      "actions": ["ALL"]
    }]
  }
  with input.authz as {"testuser": "auser"}
  with data.entroq.policy.users as [{
    "name": "auser",
    "queues": [{
      "exact": "/a/queue",
       "actions": ["ALL"]
    }]
  }]
}

test_this_user_not_found {
  not this_user
  with input.authz as {"testuser": "auser"}
  with data.entroq.policy.users as [{
    "name": "buser",
  }]
}

test_only_match_wildcard_role_queues {
  count(role_queues) == 2
  with input.authz as {"testuser": "blah"}
  with input.queues as [{
    "exact": "/ns=global/inbox",
    "actions": ["CLAIM"]
  }]
  with data.entroq.policy.users as []
  with data.entroq.policy.roles as [{
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
  with input.authz as {"testuser": "auser"}
  with input.queues as [{
    "exact": "aqueue",
    "actions": ["CLAIM"]
  }]
  with data.entroq.policy.users as []
  with data.entroq.policy.roles as [{
    "name": "*",
    "queues": [{
      "prefix": "/ns=global/",
      "actions": ["READ"],
    }]
  }]
}

test_only_extra_user_queues {
  user_queues == {{"prefix": "/ns=user/blah/", "actions": ["ALL"]}}
  with input.authz as {"testuser": "blah"}
  with data.entroq.policy.users as []
  with data.entroq.policy.roles as []
}

test_user_with_queues {
  user_queues == {
    {"prefix": "/ns=user/blah/", "actions": ["ALL"]},
    {"exact": "q1", "actions": ["READ"]},
    {"exact": "q2", "actions": ["CLAIM"]},
  }
  with input.authz as {"testuser": "blah"}
  with data.entroq.policy.users as [{
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
  user_queues == {{"prefix": "/ns=user/auser/", "actions": ["ALL"]}}
  with input.authz as {"testuser": "auser"}
  with input.queues as [{
    "exact": "aqueue",
    "actions": ["CLAIM"]
  }]
  with data.entroq.policy.users as []
  with data.entroq.policy.roles as [{
    "name": "*",
    "queues": [{
      "prefix": "/ns=global/",
      "actions": ["READ"],
    }]
  }]
}

