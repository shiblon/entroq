"""Generate a new RSA key pair for the authz example.

Writes:
  example-key.pem   -- RSA private key (PEM, used by client.py to sign JWTs)
  opa/data.json     -- OPA bundle data including inline JWKS public key,
                       IDP config (audience/issuer), and example policy
                       (users/roles).

Run this once to bootstrap the example, or again to rotate keys.
In production, replace the inline JWKS in opa/data.json with a jwks_url
pointing to your IDP's JWKS endpoint, and remove example-key.pem.
"""

import base64
import json
import os

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa

AUDIENCE = "entroq-example"
ISSUER = "entroq-example-issuer"

KEY_FILE = "example-key.pem"
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

    # Build OPA bundle data.
    data = {
        "entroq": {
            "idp": {
                # Inline JWKS -- replace with jwks_url for a real IDP.
                "jwks": json.dumps(jwks),
                "audience": AUDIENCE,
                "issuer": ISSUER,
            },
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
