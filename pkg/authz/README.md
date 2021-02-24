# Package authz

The `authz` package contains basic data structures that are used to
specify/communicate information about a request (user info, queues, actions) to
a system that can authorize that request.

It is built to make it relatively easy to make new authorizers, systems that
can handle the actual authorization, without actually implementing any of that
authorization inside EntroQ, itself. This way EntroQ is only responsible for
what it uniquely knows about: the semantics of the request itself.

In particular, EntroQ has no concept of users or authentication. It only knows
about Authorization headers in an opaque sense: it can pass them along.

# Open Policy Agent (OPA)

One implementation is the OPA HTTP subsystem. It is responsible for packaging
up an API request for an OPA server at a given URL. The URL is completely
specified, and requests/responses are given in this `authz` module.

An OPA server must be running for this to be used, and only part of the
configuration can be specified here. The core configuration files needed for
OPA to function in an EntroQ setting are located in the
[opadata/core](opadata/core) directory. Those files should be given to an OPA
server as part of its policy Rego bundles.

The system user or deployer must provide a few things:

- Data (information about user permissions, role permissions, etc.)
- An `entroq.permissions` Rego package that provides `allowed_queues`.
- An `entroq.user` Rego package that provides `username`.

## Authorization Flow

The authorization flow from EntroQ's perspective is pretty simple:

- Client
  - sends EntroQ a request over gRPC.
- EntroQ
  - packages up an authorization query.
  - extracts the value of the `Authorization` header from gRPC context.
  - sends that query, with an `Authorization` header, to OPA.
- OPA
  - returns a response containing all mismatched queues (or an error).
- EntroQ
  - proceeds or returns an error to the client based on that response.

It's a pretty simple subrequest-based authorization scheme, again, from
EntroQ's perspective.

From OPA's perspective, things can be relatively complex, because EntroQ is
doing nothing with authentication or authorization. EntroQ also provides no
information about what is allowed for whom. All of that is part of the OPA
configuration and may even be part of a larger system environment.

For example, if bearer tokens are used, they might be *opaque*, in which case
OPA would need to be configured to reach out to another service to obtain user
information for a given token (including whether that token is expired, for
example). If the bearer token is a JWT (not uncommon), OPA can simply unpack
it, verify it, and pull the `sub` field from its claims using built-in
functions for that purpose.

OPA has the ability to use data from a filesystem, and to watch for changes to
that data, so some will prefer to provide data (permitted queue specs for users
and roles) in that way. It can also reach out to another service to obtain data
specific to a user. Again, this is up to the user or deployer to configure, and
EntroQ has no opinions about how it is done.

## User-provided Policy

Examples of what username and permissions files might look like can be found in the
[opadata/example](opadata/example) directory. There, a simple (and bad!)
approach to providing the username can be found, as can an approach for parsing
provided data about users and roles.

In that example, the users and roles are organized like this:

```json
{
  "users": [{
    "name": "auser",
    "roles": ["role1", "role2"],
    "queues": [{
      "exact": "aqueue",
      "actions": ["*"]
    }, {
      "prefix": "/mystuff/",
      "actions": ["CLAIM", "CHANGE", "DELETE"]
    }]
  }],
  "roles": [{
    "name": "*",
    "queues": [{
      "prefix": "/free-for-all/",
      "actions": ["*"]
    }],
  }]
}
```

If this structure suits your data purposes, feel free to copy the example permissions
from the [example directory](opadata/example/example-permissions.rego).

The username should usually be taken from `input.identity`, following
instructions in the OPA [security documentation](https://www.openpolicyagent.org/docs/latest/security/).

EntroQ will, when using the OPA HTTP module, pass an Authorization header from
the gRPC call context through to OPA during an authorization request. This can
be parsed by OPA by telling it to use this with appropriate `--authentication`
and `--authorization` settings for your use case.

## User-Provided Data

There are many ways to get permissions data into OPA, which is one of the
reasons that EntroQ has no opinion about it. Shape the data how you like, get
it to OPA how you like, and craft your `entroq.permissions` package to convert
that data (along with the current user credentials) into a set of
`allowed_queues` queue specs that are allowed for this user. The core logic
does the rest.

Permissions data is one of the fastest changing things in an authorization
context like this. OPA can watch files for changes, so you might try running it
in a container with a data bundle mounted into it, for example. This idea is
compatible with many systems, such as Kubernetes secrets.

However you provide data and configuration to OPA, simply ensure that the core
files are also loaded.
