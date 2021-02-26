package entroq.policy

users := [
  {
    "name": "auser",
    "roles": ["role1", "role2"],
    "queues": [{
      "prefix": "/users/auser/",
      "actions": ["*"]
    }]
  }
]

roles := [
  {
    "name": "*",
    "queues": [{
      "prefix": "/global/",
      "actions": ["READ"]
    }]
  }
]
