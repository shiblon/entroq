"""Integration test for the authz-example stack.

Requires the docker-compose stack to be running:
    docker compose up --build

Run:
    pip install requests pyjwt cryptography
    python test.py

Exits 0 on all pass, 1 on any failure.
"""

import json
import sys
from datetime import datetime, timezone, timedelta
from pathlib import Path

import jwt
import requests

ENTROQ_URL = "http://localhost:9100"
KEY_FILE = Path(__file__).parent / "example-key.pem"
DATA_FILE = Path(__file__).parent / "opa" / "data.json"

AUDIENCE = "entroq-example"
ISSUER = "entroq-example-issuer"

PASS = "\033[32mPASS\033[0m"
FAIL = "\033[31mFAIL\033[0m"


def make_token(subject: str, expiry: timedelta = timedelta(hours=1)) -> str:
    private_key = KEY_FILE.read_text()
    now = datetime.now(timezone.utc)
    return jwt.encode({
        "sub": subject,
        "aud": AUDIENCE,
        "iss": ISSUER,
        "iat": now,
        "exp": now + expiry,
    }, private_key, algorithm="RS256")


def session_for(token: str | None) -> requests.Session:
    s = requests.Session()
    if token:
        s.headers["Authorization"] = f"Bearer {token}"
    return s


def do_insert(session: requests.Session, queue: str, value: object = "test") -> requests.Response:
    return session.post(f"{ENTROQ_URL}/api/v0/modify", json={
        "claimantId": "test-client",
        "inserts": [{"queue": queue, "value": json.dumps(value)}],
    })


def do_read(session: requests.Session, queue: str) -> requests.Response:
    return session.get(f"{ENTROQ_URL}/api/v0/tasks", params={"queue": queue})


failures = 0


def check(label: str, resp: requests.Response, expect_ok: bool) -> None:
    global failures
    got_ok = resp.ok
    passed = got_ok == expect_ok
    if passed:
        print(f"  [{PASS}] {label}")
    else:
        direction = "allowed" if got_ok else "denied"
        expected = "allowed" if expect_ok else "denied"
        print(f"  [{FAIL}] {label}")
        print(f"           expected {expected}, got {direction} (HTTP {resp.status_code})")
        if not got_ok:
            print(f"           {resp.text[:200]}")
        failures += 1


def main() -> None:
    # Verify stack is reachable.
    try:
        requests.get(f"{ENTROQ_URL}/api/v0/time", timeout=5).raise_for_status()
    except Exception as e:
        print(f"Cannot reach EntroQ at {ENTROQ_URL}: {e}", file=sys.stderr)
        print("Is 'docker compose up' running?", file=sys.stderr)
        sys.exit(1)

    alice = session_for(make_token("alice"))
    bob = session_for(make_token("bob"))
    expired = session_for(make_token("alice", expiry=timedelta(seconds=-1)))
    no_token = session_for(None)
    wrong_iss = session_for(jwt.encode({
        "sub": "alice", "aud": AUDIENCE, "iss": "wrong-issuer",
        "iat": datetime.now(timezone.utc),
        "exp": datetime.now(timezone.utc) + timedelta(hours=1),
    }, KEY_FILE.read_text(), algorithm="RS256"))

    print("\nPersonal namespace (auto-granted to everyone):")
    check("alice: insert /users/alice/inbox",    do_insert(alice, "/users/alice/inbox"), True)
    check("bob:   insert /users/bob/inbox",      do_insert(bob,   "/users/bob/inbox"),   True)
    check("alice: insert /users/bob/inbox",      do_insert(alice, "/users/bob/inbox"),   False)
    check("bob:   insert /users/alice/inbox",    do_insert(bob,   "/users/alice/inbox"), False)

    print("\nShared queues:")
    check("alice: insert /shared/work (writers role)", do_insert(alice, "/shared/work"),  True)
    check("alice: read   /shared/inbox",               do_read(alice,   "/shared/inbox"), True)
    check("bob:   read   /shared/inbox",               do_read(bob,     "/shared/inbox"), True)
    check("bob:   insert /shared/work (no writers)",   do_insert(bob,   "/shared/work"),  False)

    print("\nToken validity:")
    check("no token:     insert /users/alice/inbox", do_insert(no_token,  "/users/alice/inbox"), False)
    check("expired:      insert /users/alice/inbox", do_insert(expired,   "/users/alice/inbox"), False)
    check("wrong issuer: insert /users/alice/inbox", do_insert(wrong_iss, "/users/alice/inbox"), False)

    print()
    if failures:
        print(f"{failures} test(s) FAILED.")
        sys.exit(1)
    else:
        print("All tests passed.")


if __name__ == "__main__":
    main()
