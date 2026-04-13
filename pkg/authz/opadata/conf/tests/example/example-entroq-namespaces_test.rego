# regal ignore:directory-package-mismatch
package entroq.permissions

import rego.v1

# regal ignore:test-outside-test-package
test_user_personal_namespace if {
	user_namespaces == {{"prefix": "/users/blah/", "actions": ["ALL"]}} with data.entroq.user.name as "blah"
		with data.entroq.policy.users as []
}

# regal ignore:test-outside-test-package
test_user_explicit_namespaces if {
	user_namespaces == {
		{"prefix": "/users/auser/", "actions": ["ALL"]},
		{"exact": "ns1", "actions": ["READ"]},
	} with data.entroq.user.name as "auser"
		with data.entroq.policy.users as [{
			"name": "auser",
			"namespaces": [{"exact": "ns1", "actions": ["READ"]}],
		}]
}

# regal ignore:test-outside-test-package
test_role_namespaces if {
	role_namespaces == {{"prefix": "/global/", "actions": ["READ"]}} with data.entroq.user.name as "auser"
		with data.entroq.policy.users as [{"name": "auser", "roles": ["role1"]}]
		with data.entroq.policy.roles as [{
			"name": "role1",
			"namespaces": [{"prefix": "/global/", "actions": ["READ"]}],
		}]
}

# regal ignore:test-outside-test-package
test_wildcard_role_namespaces if {
	role_namespaces == {{"prefix": "/public/", "actions": ["READ"]}} with data.entroq.user.name as "auser"
		with data.entroq.policy.users as []
		with data.entroq.policy.roles as [{
			"name": "*",
			"namespaces": [{"prefix": "/public/", "actions": ["READ"]}],
		}]
}

# regal ignore:test-outside-test-package
test_allowed_namespaces_union if {
	allowed_namespaces == {
		{"prefix": "/users/auser/", "actions": ["ALL"]},
		{"exact": "ns1", "actions": ["READ"]},
		{"prefix": "/public/", "actions": ["READ"]},
	} with data.entroq.user.name as "auser"
		with data.entroq.policy.users as [{
			"name": "auser",
			"namespaces": [{"exact": "ns1", "actions": ["READ"]}],
			"roles": ["*"],
		}]
		with data.entroq.policy.roles as [{
			"name": "*",
			"namespaces": [{"prefix": "/public/", "actions": ["READ"]}],
		}]
}
