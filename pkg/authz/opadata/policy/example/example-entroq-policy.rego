# regal ignore:directory-package-mismatch
package entroq.policy

import rego.v1

users := [{
	"name": "auser",
	"roles": ["role1", "role2"],
	"queues": [{
		"prefix": "/users/auser/",
		"actions": ["*"],
	}],
	"namespaces": [{
		"prefix": "/users/auser/",
		"actions": ["*"],
	}],
}]

roles := [{
	"name": "*",
	"queues": [{
		"prefix": "/global/",
		"actions": ["READ"],
	}],
	"namespaces": [{
		"prefix": "/global/",
		"actions": ["READ"],
	}],
}]
