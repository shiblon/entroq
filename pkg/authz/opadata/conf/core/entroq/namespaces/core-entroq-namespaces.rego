package entroq.namespaces

import rego.v1

# This package operates on namespaces and sets of namespace specs.
# Namespace specs are expected to look like this:
# {
#   "exact": "name/of/namespace",
#   "actions": ["READ", "INSERT"],
# }

# name_match returns true if "want" is matched by "can".
name_match(want, can) if {
	want.exact == can.exact
}

name_match(want, can) if {
	startswith(want.exact, can.prefix)
}

name_match(want, can) if {
	startswith(want.prefix, can.prefix)
}

# is_named reports whether a namespace spec actually names something: a non-empty
# exact name or a non-empty prefix. A spec that names nothing can never be
# authorized (there is nothing to hold a grant on), and an empty value would
# otherwise match a wildcard prefix via startswith("", ""). Callers use this to
# reject empty requests outright.
is_named(spec) if spec.exact != ""

is_named(spec) if spec.prefix != ""

# has_wildcard indicates whether a set contains a wildcard.
has_wildcard(actions) if {
	some wildcard in ["*", "ALL", "ANY"]
	actions[wildcard]
}

# actions_left returns actions in "want" that are not covered by "can".
actions_left(want, can) := {y | y := (want - can)[_]; not has_wildcard(can)}

# disallowed returns a set of namespace specs that are not covered by allowances.
disallowed(want, can) := {n |
	some want_n in want
	want_actions := {a | some a in want_n.actions}
	can_actions := {a | some an in can; name_match(want_n, an); some a in an.actions}

	left := actions_left(want_actions, can_actions)
	count(left) > 0

	n := object.union(object.remove(want_n, ["actions"]), {"actions": left})
}
