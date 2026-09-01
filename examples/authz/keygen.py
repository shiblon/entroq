"""Generate a new RSA key pair for the authz example.

Writes:
  example-key.pem   -- RSA private key (PEM, used by client.py to sign JWTs)
  jwks.json         -- public key read by the EntroQ JWT authenticator
  opa/data.json     -- OPA authorization policy (users/roles)

Run this once to bootstrap the example, or again to rotate keys.
In production, configure --auth_jwks_url for your IDP and remove both local key
files.
"""

import base64
import json
import os

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa

AUDIENCE = "entroq-example"
ISSUER = "entroq-example-issuer"

KEY_FILE = "example-key.pem"
JWKS_FILE = "jwks.json"
DATA_FILE = os.path.join("opa", "data.json")


def b64url(n: int) -> str:
    """Encode a big integer as base64url (no padding), as required by JWKS."""
    length = (n.bit_length() + 7) // 8
    return base64.urlsafe_b64encode(n.to_bytes(length, "big")).rstrip(b"=").decode()


def main():
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    pub = key.public_key()
    pn = pub.public_numbers()

    # Write private key.
    with open(KEY_FILE, "wb") as f:
        f.write(key.private_bytes(
            serialization.Encoding.PEM,
            serialization.PrivateFormat.TraditionalOpenSSL,
            serialization.NoEncryption(),
        ))
    print(f"Wrote {KEY_FILE}")

    # Build JWKS from public key.
    jwks = {
        "keys": [{
            "kty": "RSA",
            "use": "sig",
            "alg": "RS256",
            "kid": "example-key",
            "n": b64url(pn.n),
            "e": b64url(pn.e),
        }]
    }

    with open(JWKS_FILE, "w") as f:
        json.dump(jwks, f, indent=2)
        f.write("\n")
    print(f"Wrote {JWKS_FILE}")

    # Build OPA authorization data. Authentication settings stay with EntroQ.
    data = {
        "entroq": {
            "policy": {
                "users": [
                    {
                        "name": "alice",
                        "roles": ["writers"],
                        "queues": [
                            {"prefix": "/users/alice/", "actions": ["*"]},
                        ],
                    },
                    {
                        "name": "bob",
                        "roles": [],
                        "queues": [
                            {"exact": "/shared/inbox", "actions": ["READ", "CLAIM"]},
                        ],
                    },
                ],
                "roles": [
                    {
                        "name": "writers",
                        "queues": [
                            {"prefix": "/shared/", "actions": ["INSERT", "READ"]},
                        ],
                    },
                ],
            },
        }
    }

    with open(DATA_FILE, "w") as f:
        json.dump(data, f, indent=2)
        f.write("\n")
    print(f"Wrote {DATA_FILE}")


if __name__ == "__main__":
    main()
