package entroq.queues

test_name_match_exact_exact {
  name_match({"exact": "/very/exact/"}, {"exact": "/very/exact/"})

  not name_match({"exact": "/something/"}, {"exact", "/something"})
  not name_match({"exact": ""}, {"exact", ""})
}

test_name_match_exact_prefix {
  name_match({"exact": "/need/all/of/this"}, {"prefix": "/need/"})
  name_match({"exact": "/need/stuff"}, {"prefix": ""})
  name_match({"exact": ""}, {"prefix": ""})
  name_match({"exact": "/"}, {"prefix": "/"})

  not name_match({"exact": "/need/stuff"}, {"prefix": "/need/t"})
  not name_match({"exact": ""}, {"prefix": "/"})
}

test_name_match_prefix_prefix {
  name_match({"prefix": "/start/here"}, {"prefix": "/start/"})
  name_match({"prefix": "/start/here"}, {"prefix": ""})
  name_match({"prefix": ""}, {"prefix": ""})

  not name_match({"prefix": ""}, {"prefix": "/"})
  not name_match({"prefix": "/hey"}, {"prefix": "/hi"})
}


test_actions_left {
  actions_left({"CLAIM", "READ"}, {"CLAIM"}) == {"READ"}
  actions_left({"CLAIM", "READ", "INSERT"}, {"ALL"}) == set()
  actions_left({"ALL"}, {"ALL"}) == set()
  actions_left({"CLAIM", "READ", "INSERT"}, {"DELETE"}) == {"CLAIM", "READ", "INSERT"}
  actions_left(set(), {"READ"}) == set()
  actions_left({"CLAIM"}, set()) == {"CLAIM"}
}


test_disallowed {
  disallowed([
    {"exact": "/let/me/in", "actions": ["CLAIM", "CHANGE", "DELETE"]},
    {"exact": "/just/insert", "actions": ["INSERT"]}
  ],[
    {"prefix": "/let/me/", "actions": ["ALL"]},
    {"exact": "/just/insert", "actions": ["READ"]}
  ]) == {
    {"exact": "/just/insert", "actions": {"INSERT"}}
  }

  disallowed([
    {"exact": "/let/me/in", "actions": ["CLAIM", "CHANGE", "DELETE"]},
    {"exact": "/just/insert", "actions": ["INSERT"]}
  ], [
    {"prefix": "/let/", "actions": ["CLAIM", "CHANGE"]},
    {"prefix": "/just", "actions": ["ALL"]}
  ]) == {
    {"exact": "/let/me/in", "actions": {"DELETE"}}
  }

  disallowed([
    {"exact": "/let/me/in", "actions": ["CLAIM", "CHANGE", "DELETE"]}
  ], [
    {"prefix": "/let/", "actions": ["CLAIM"]}
  ]) == {
    {"exact": "/let/me/in", "actions": {"CHANGE", "DELETE"}}
  }

  disallowed([
    {"exact": "/let/me/in", "actions": ["CLAIM", "CHANGE", "DELETE"]},
    {"exact": "/let/me/out", "actions": ["CLAIM", "CHANGE", "DELETE"]}
  ], [
    {"prefix": "/let/me/", "actions": ["CLAIM", "CHANGE"]},
    {"exact": "/let/me/in", "actions": ["DELETE"]},
  ]) == {
    {"exact": "/let/me/out", "actions": {"DELETE"}}
  }
}
