package entroq.permissions

import rego.v1

# Helpers: a minimal mesh document and a couple of identities.

mock_mesh := {
	"initialized": true,
	"identities": {
		"system:serviceaccount:payments:svc-a": {"labels": {"group": "frontend", "team": "payments"}},
		"system:serviceaccount:payments:svc-b": {"labels": {"group": "backend"}},
	},
	"queues": [
		{
			"pattern": "/payments/svc-b/inbox",
			"matchType": "Exact",
			"allowedCallers": [
				{"group": "frontend"},
				{"group": "internal-tools"},
			],
		},
		{
			"pattern": "/shared/",
			"matchType": "Prefix",
			"allowedCallers": [
				{"group": "backend"},
			],
		},
	],
}

# own_prefix: correctly parsed from the SA subject.
test_own_prefix_parsed if {
	own_prefix == "/payments/svc-a/"
		with data.entroq.user.name as "system:serviceaccount:payments:svc-a"
}

# own_prefix: undefined for a non-SA subject (fails closed).
test_own_prefix_undefined_for_non_sa if {
	not own_prefix
		with data.entroq.user.name as "alice"
}

# Auto-grant: SA always gets ALL on its own prefix.
test_auto_grant_present if {
	{"prefix": "/payments/svc-a/", "actions": ["ALL"]} in allowed_queues
		with data.entroq.user.name as "system:serviceaccount:payments:svc-a"
		with data.mesh as mock_mesh
}

# Mesh grant: svc-a satisfies svc-b inbox policy (group=frontend matches).
test_mesh_grant_exact_match if {
	{"exact": "/payments/svc-b/inbox", "actions": ["ALL"]} in allowed_queues
		with data.entroq.user.name as "system:serviceaccount:payments:svc-a"
		with data.mesh as mock_mesh
}

# Mesh grant: svc-b satisfies shared prefix policy (group=backend matches).
test_mesh_grant_prefix_match if {
	{"prefix": "/shared/", "actions": ["ALL"]} in allowed_queues
		with data.entroq.user.name as "system:serviceaccount:payments:svc-b"
		with data.mesh as mock_mesh
}

# Mesh grant absent: svc-b does not satisfy svc-b inbox policy (not frontend/internal-tools).
test_mesh_grant_denied if {
	not {"exact": "/payments/svc-b/inbox", "actions": ["ALL"]} in allowed_queues
		with data.entroq.user.name as "system:serviceaccount:payments:svc-b"
		with data.mesh as mock_mesh
}

# Uninitialized mesh: no mesh grants emitted, only auto-grant.
test_uninitialized_suppresses_mesh_grants if {
	queues := allowed_queues
		with data.entroq.user.name as "system:serviceaccount:payments:svc-a"
		with data.mesh as object.union(mock_mesh, {"initialized": false})
	queues == {{"prefix": "/payments/svc-a/", "actions": ["ALL"]}}
}

# Unknown identity: no mesh grants for a subject not in data.mesh.identities.
test_unknown_identity_no_mesh_grants if {
	queues := allowed_queues
		with data.entroq.user.name as "system:serviceaccount:other:unknown"
		with data.mesh as mock_mesh
	queues == {{"prefix": "/other/unknown/", "actions": ["ALL"]}}
}

# AND semantics within a matcher: both keys must match.
test_and_semantics_within_matcher if {
	mesh := object.union(mock_mesh, {"queues": [{
		"pattern": "/strict/queue",
		"matchType": "Exact",
		"allowedCallers": [{"group": "frontend", "team": "payments"}],
	}]})

	# svc-a has both group=frontend and team=payments -- allowed.
	{"exact": "/strict/queue", "actions": ["ALL"]} in allowed_queues
		with data.entroq.user.name as "system:serviceaccount:payments:svc-a"
		with data.mesh as mesh

	# svc-b has group=backend only -- denied.
	not {"exact": "/strict/queue", "actions": ["ALL"]} in allowed_queues
		with data.entroq.user.name as "system:serviceaccount:payments:svc-b"
		with data.mesh as mesh
}
