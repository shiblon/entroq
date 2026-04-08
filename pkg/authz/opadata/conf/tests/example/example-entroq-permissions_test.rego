package entroq.permissions

import rego.v1

test_this_user_found if {
	this_user == {
		"name": "auser",
		"queues": [{
			"exact": "/a/queue",
			"actions": ["ALL"],
		}],
	}
	with data.entroq.user.username as "auser"
	with data.entroq.policy.users as [{
		"name": "auser",
		"queues": [{
			"exact": "/a/queue",
			"actions": ["ALL"],
		}],
	}]
}

test_this_user_not_found if {
	not this_user
	with data.entroq.user.username as "auser"
	with data.entroq.policy.users as [{
		"name": "buser",
	}]
}

test_only_match_wildcard_role_queues if {
	count(role_queues) == 2
	with data.entroq.user.username as "blah"
	with input.queues as [{
		"exact": "/ns=global/inbox",
		"actions": ["CLAIM"],
	}]
	with data.entroq.policy.users as []
	with data.entroq.policy.roles as [{
		"name": "*",
		"queues": [
			{
				"prefix": "/ns=global/",
				"actions": ["READ"],
			},
			{
				"exact": "/ns=global/inbox",
				"actions": ["CLAIM"],
			},
		],
	}]
}

test_only_match_wildcard_role_queues_single if {
	count(role_queues) == 1
	with data.entroq.user.username as "auser"
	with input.queues as [{
		"exact": "aqueue",
		"actions": ["CLAIM"],
	}]
	with data.entroq.policy.users as []
	with data.entroq.policy.roles as [{
		"name": "*",
		"queues": [{
			"prefix": "/ns=global/",
			"actions": ["READ"],
		}],
	}]
}

test_only_extra_user_queues if {
	user_queues == {{"prefix": "/users/blah/", "actions": ["ALL"]}}
	with data.entroq.user.username as "blah"
	with data.entroq.policy.users as []
	with data.entroq.policy.roles as []
}

test_user_with_queues if {
	user_queues == {
		{"prefix": "/users/blah/", "actions": ["ALL"]},
		{"exact": "q1", "actions": ["READ"]},
		{"exact": "q2", "actions": ["CLAIM"]},
	}
	with data.entroq.user.username as "blah"
	with data.entroq.policy.users as [{
		"name": "blah",
		"queues": [
			{
				"exact": "q1",
				"actions": ["READ"],
			},
			{
				"exact": "q2",
				"actions": ["CLAIM"],
			},
		],
	}]
}

test_no_matching_user_queues if {
	user_queues == {{"prefix": "/users/auser/", "actions": ["ALL"]}}
	with data.entroq.user.username as "auser"
	with input.queues as [{
		"exact": "aqueue",
		"actions": ["CLAIM"],
	}]
	with data.entroq.policy.users as []
	with data.entroq.policy.roles as [{
		"name": "*",
		"queues": [{
			"prefix": "/ns=global/",
			"actions": ["READ"],
		}],
	}]
}
