# regal ignore:directory-package-mismatch
package entroq.authz

import rego.v1

# Tests for Namespace Authorization.
# These mirror the Queue tests to ensure consistency across resources.

# regal ignore:test-outside-test-package
test_input_matches_no_namespaces if {
	failed_namespaces == {{
		"exact": "ans",
		"actions": {"INSERT", "READ"},
	}} with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_namespaces as set()
		with input.namespaces as [{
			"exact": "ans",
			"actions": ["INSERT", "READ"],
		}]
}

# regal ignore:test-outside-test-package
test_one_exact_namespace_match if {
	count(failed_namespaces) == 0 with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_namespaces as [{
			"exact": "/users/auser/docs",
			"actions": ["INSERT", "READ"],
		}]
		with input.namespaces as [{
			"exact": "/users/auser/docs",
			"actions": ["INSERT", "READ"],
		}]
}

# regal ignore:test-outside-test-package
test_one_exact_namespace_missing_actions if {
	failed_namespaces == {{
		"exact": "/users/auser/docs",
		"actions": {"INSERT"},
	}} with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_namespaces as [{
			"exact": "/users/auser/docs",
			"actions": ["READ"],
		}]
		with input.namespaces as [{
			"exact": "/users/auser/docs",
			"actions": ["INSERT", "READ"],
		}]
}

# regal ignore:test-outside-test-package
test_one_prefix_namespace_match if {
	count(failed_namespaces) == 0 with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_namespaces as [{
			"prefix": "/users/auser/",
			"actions": ["ALL"],
		}]
		with input.namespaces as [{
			"exact": "/users/auser/docs",
			"actions": ["INSERT", "READ"],
		}]
}

# regal ignore:test-outside-test-package
test_allow_false_if_namespace_fails if {
	not allow with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as [{
			"exact": "q1",
			"actions": ["ALL"],
		}]
		with data.entroq.permissions.allowed_namespaces as set()
		with input.queues as [{"exact": "q1", "actions": ["READ"]}]
		with input.namespaces as [{"exact": "ns1", "actions": ["READ"]}]
}

# regal ignore:test-outside-test-package
test_allow_true_if_both_pass if {
	allow with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as [{
			"exact": "q1",
			"actions": ["ALL"],
		}]
		with data.entroq.permissions.allowed_namespaces as [{
			"exact": "ns1",
			"actions": ["ALL"],
		}]
		with input.queues as [{"exact": "q1", "actions": ["READ"]}]
		with input.namespaces as [{"exact": "ns1", "actions": ["READ"]}]
}

# regal ignore:test-outside-test-package
test_allow_false_if_queue_fails_even_if_namespace_passes if {
	not allow with data.entroq.user.name as "auser"
		with data.entroq.permissions.allowed_queues as set()
		with data.entroq.permissions.allowed_namespaces as [{
			"exact": "ns1",
			"actions": ["ALL"],
		}]
		with input.queues as [{"exact": "q1", "actions": ["READ"]}]
		with input.namespaces as [{"exact": "ns1", "actions": ["READ"]}]
}
