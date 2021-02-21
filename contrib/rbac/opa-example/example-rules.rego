package app.authz

username = input.authz.token # TODO: parse JWT or something, strip "Bearer" off to figure that out.

# A set of users with this username.
this_user[u] {
    u := data.users[_]
    u.name == username
}

# Roles that can be used to allow this user.
user_role_names := {"*"} | {x | x = this_user[_].roles[_]}
user_roles[r] {
    r := data.roles[_]
    user_role_names[r.name]
}

user_queues[u.queues[_]] {
    u := this_user[_]
}

role_queues[r.queues[_]] {
    r := data.roles[_]
    user_role_names[r.name]
}

possible_queues := role_queues | user_queues

name_matches(want, can) {
    want.exact == can.exact
}
name_matches(want, can) {
    startswith(want.exact, can.prefix)
}
name_matches(want, can) {
    startswith(want.prefix, can.prefix)
}

actions_satisfied(want, can) {
    can["ALL"]
}
actions_satisfied(want, can) {
    count(want - can) == 0
}

# Match exact to allowed prefix.
queue_matches[q] {
    some qi, mi
    my_q := input.queues[qi]
    allow_q := possible_queues[mi]
    q := {"i": qi, "want": {x | x := my_q.actions[_]}, "can": {x | x := allow_q.actions[_]}}
    name_matches(my_q, allow_q)
    actions_satisfied(q.want, q.can)
}

missing[q] {
    want := {x | x := numbers.range(0, count(input.queues)-1)[_]}
    can := {x | x := queue_matches[_].i}
    need := want - can
    q := input.queues[i]
    need[i]
}

default allow = false
allow {
    count(missing) == 0
}

