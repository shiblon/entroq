# regal ignore:directory-package-mismatch
package entroq.permissions

import rego.v1

import data.entroq.user as equser

# Here we're using the policy package as "user and role data".
# There are other ways of doing it, like having actual data pushed into the
# service as a document, and then relying on that. This approach allows the
# data to be specified in Rego. See the example "policy" implementation outside
# of this config tree (outside to allow testing without relying on data from a
# file).
import data.entroq.policy

# The user with this username. Rego will error out if there is more than one.
this_user := u if {
	some u in policy.users
	u.name == equser.name
}

user_queues contains qs if {
	some qs in this_user.queues
}

user_namespaces contains ns if {
	some ns in this_user.namespaces
}

# Auto-grant every authenticated user full access to their personal /users/<name>/ prefix.
user_queues contains {"prefix": concat("", ["/users/", equser.name, "/"]), "actions": ["ALL"]}

user_namespaces contains {"prefix": concat("", ["/users/", equser.name, "/"]), "actions": ["ALL"]}

role_queues contains q if {
	some r in policy.roles
	({x | some x in this_user.roles} | {"*"})[r.name]
	some q in r.queues
}

role_namespaces contains n if {
	some r in policy.roles
	({x | some x in this_user.roles} | {"*"})[r.name]
	some n in r.namespaces
}

allowed_queues contains q if {
	some q in (user_queues | role_queues)
}

allowed_namespaces contains n if {
	some n in (user_namespaces | role_namespaces)
}
