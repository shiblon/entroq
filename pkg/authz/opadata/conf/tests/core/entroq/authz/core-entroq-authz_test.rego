package entroq.authz

import rego.v1

# Tests mock data.entroq.user.name and data.entroq.permissions.allowed_queues
# directly, bypassing JWT validation and the permissions implementation.
# This keeps the core authz tests independent of any particular IDP or
# policy data format.

test_input_matches_no_queues if {
	failed == {{
		"exact": "aqueue",
		"actions": {"CLAIM", "DELETE"},
	}} with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as set()
		with input.queues as [{
			"exact": "aqueue",
			"actions": ["CLAIM", "DELETE"],
		}]
}

test_one_exact_exact_user_match if {
	count(errors) == 0 with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as [{
			"exact": "/users/auser/inbox",
			"actions": ["DELETE", "CLAIM"],
		}]
		with input.queues as [{
			"exact": "/users/auser/inbox",
			"actions": ["CLAIM", "DELETE"],
		}]
}

test_one_exact_match_missing_actions if {
	failed == {{
		"exact": "/users/auser/inbox",
		"actions": {"DELETE"},
	}} with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as [{
			"exact": "/users/auser/inbox",
			"actions": ["CLAIM"],
		}]
		with input.queues as [{
			"exact": "/users/auser/inbox",
			"actions": ["CLAIM", "DELETE"],
		}]
}

test_one_exact_prefix_match if {
	count(errors) == 0 with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as [{
			"prefix": "/users/auser/",
			"actions": ["ALL"],
		}]
		with input.queues as [{
			"exact": "/users/auser/inbox",
			"actions": ["CLAIM", "DELETE"],
		}]
}

test_one_prefix_prefix_match if {
	count(errors) == 0 with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as [{
			"prefix": "/users/auser/",
			"actions": ["ALL"],
		}]
		with input.queues as [{
			"prefix": "/users/auser/inbox",
			"actions": ["CLAIM", "DELETE"],
		}]
}

test_exact_against_multiple_matches if {
	count(errors) == 0 with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as [
			{"prefix": "/users/auser/", "actions": ["READ"]},
			{"exact": "/users/auser/inbox", "actions": ["CLAIM"]},
			{"exact": "/users/auser/inbox", "actions": ["DELETE"]},
		]
		with input.queues as [{
			"exact": "/users/auser/inbox",
			"actions": ["CLAIM", "DELETE", "READ"],
		}]
}

test_exact_against_partial_actions if {
	failed == {{
		"exact": "/users/auser/inbox",
		"actions": {"CLAIM"},
	}} with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as [
			{"prefix": "/users/auser/", "actions": ["READ"]},
			{"exact": "/users/auser/inbox", "actions": ["DELETE"]},
		]
		with input.queues as [{
			"exact": "/users/auser/inbox",
			"actions": ["CLAIM", "DELETE", "READ"],
		}]
}

test_prefix_prefix_match if {
	count(errors) == 0 with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as [{
			"prefix": "/auser/",
			"actions": ["ALL"],
		}]
		with input.queues as [{
			"prefix": "/auser/stuff",
			"actions": ["CLAIM"],
		}]
}

test_prefix_prefix_nomatch if {
	count(errors) >= 0 with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as [{
			"prefix": "/auser/stuff/",
			"actions": ["ALL"],
		}]
		with input.queues as [{
			"prefix": "/auser/stuff",
			"actions": ["CLAIM"],
		}]
}

test_allow_true if {
	allow with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as [{
			"exact": "/users/auser/inbox",
			"actions": ["CLAIM", "DELETE"],
		}]
		with input.queues as [{
			"exact": "/users/auser/inbox",
			"actions": ["CLAIM", "DELETE"],
		}]
}

test_allow_false_no_matching_queues if {
	not allow with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as set()
		with input.queues as [{
			"exact": "/users/auser/inbox",
			"actions": ["CLAIM"],
		}]
}

test_allow_false_missing_actions if {
	not allow with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as [{
			"exact": "/users/auser/inbox",
			"actions": ["READ"],
		}]
		with input.queues as [{
			"exact": "/users/auser/inbox",
			"actions": ["CLAIM", "READ"],
		}]
}

test_allow_false_empty_allowed_queues if {
	not allow with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as set()
		with data.entroq.permissions.allowed_namespaces as set()
		with input.queues as []
}

test_claimant_match_allow if {
	allow with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as [{"exact": "/q", "actions": ["CLAIM"]}]
		with data.entroq.permissions.allowed_namespaces as set()
		with input.queues as [{"exact": "/q", "actions": ["CLAIM"]}]
		with input.claimant_id as "auser#abc123"
}

test_claimant_mismatch_deny if {
	not allow with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as [{"exact": "/q", "actions": ["CLAIM"]}]
		with data.entroq.permissions.allowed_namespaces as set()
		with input.queues as [{"exact": "/q", "actions": ["CLAIM"]}]
		with input.claimant_id as "otheruser#abc123"
}

test_claimant_empty_allow if {
	allow with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as [{"exact": "/q", "actions": ["CLAIM"]}]
		with data.entroq.permissions.allowed_namespaces as set()
		with input.queues as [{"exact": "/q", "actions": ["CLAIM"]}]
		with input.claimant_id as ""
}

test_claimant_set_no_user_allow if {
	# Unauthenticated: user.name is undefined, claimant_id passes through.
	allow with data.entroq.permissions.allowed_queues as [{"exact": "/q", "actions": ["CLAIM"]}]
		with data.entroq.permissions.allowed_namespaces as set()
		with input.queues as [{"exact": "/q", "actions": ["CLAIM"]}]
		with input.claimant_id as "anyone#abc123"
}

# A request naming an empty queue is denied outright, even for an admin holding
# a wildcard (empty-prefix) grant: no queue, no perms.
test_empty_queue_request_denied_even_with_wildcard if {
	not allow with data.entroq.user.name as "admin"
		with data.entroq.permissions.allowed_queues as [{"prefix": "", "actions": ["ALL"]}]
		with data.entroq.permissions.allowed_namespaces as set()
		with input.queues as [{"exact": "", "actions": ["DELETE"]}]
}

# The wildcard grant is untouched: it still authorizes any actually-named queue.
test_wildcard_still_allows_named_queue if {
	allow with data.entroq.user.name as "admin"
		with data.entroq.permissions.allowed_queues as [{"prefix": "", "actions": ["ALL"]}]
		with data.entroq.permissions.allowed_namespaces as set()
		with input.queues as [{"exact": "/real/queue", "actions": ["DELETE", "CLAIM"]}]
}

# Same for namespaces: an empty namespace request is denied even with a wildcard.
test_empty_namespace_request_denied_even_with_wildcard if {
	not allow with data.entroq.user.name as "admin"
		with data.entroq.permissions.allowed_queues as set()
		with data.entroq.permissions.allowed_namespaces as [{"prefix": "", "actions": ["ALL"]}]
		with input.namespaces as [{"exact": "", "actions": ["READ"]}]
}

test_wildcard_still_allows_named_namespace if {
	allow with data.entroq.user.name as "admin"
		with data.entroq.permissions.allowed_queues as set()
		with data.entroq.permissions.allowed_namespaces as [{"prefix": "", "actions": ["ALL"]}]
		with input.namespaces as [{"exact": "/real/ns", "actions": ["READ"]}]
}
