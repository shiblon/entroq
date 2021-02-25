package entroq.user

# ONLY FOR TESTING
username = u {
  u := input.authz.testuser
  u; u != ""
}

jwt_body = b {
  b := io.jwt.decode(input.authz.credentials)[1]
  input.authz.type == "Bearer"
  input.authz.credentials
}

username = u {
  u := jwt_body.sub
}

# ONLY FOR TESTING: Normally you would also want to *validate* this token.

