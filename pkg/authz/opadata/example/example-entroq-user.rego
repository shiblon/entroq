package entroq.user

# ONLY FOR TESTING
username = u {
  u := input.authz.testuser
  u != ""
}

# ONLY FOR TESTING: Normally you would also want to *validate* this token.

