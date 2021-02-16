package envoy.authz

import input.attributes.request.http as http_request

default allow = false

token = {"value": valid, "payload": payload} {
  [_, encoded] := split(http_request.headers.authorization, " ")
  [valid, _, payload] := io.jwt.decode_verify(encoded, {"secret": "secret"})
}

allow {
  is_token_valid
  action_allowed
}

is_token_valid {
  token.value
  now := time.now_ns() / 1e9
  token.payload.nbf <= now
  now < token.payload.exp
}

action_allowed {
  http_request.method == "GET"
  token.payload.role == "guest"
  glob.match("/people*", [], http_request.path)
}

action_allowed {
  http_request.method == "GET"
  token.payload.role == "admin"
  glob.match("/people*", [], http_request.path)
}

action_allowed {
  http_request.method == "POST"
  token.payload.role == "admin"
  glob.match("/people", [], http_request.path)
  lower(input.parsed_bpdy.firstname) != base64url.decode(token.payload.sub)
}
