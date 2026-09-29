package entroq.user

import rego.v1

# Rego expression profiles use a pre-resolved identity so queue-policy work can
# be measured separately from JWT verification and JWKS transport.
name := input.profile_user
