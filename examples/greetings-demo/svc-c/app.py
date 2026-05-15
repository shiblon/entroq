"""svc-c: compliment service. Returns a random compliment. No outbound calls."""
import json
import random
from http.server import BaseHTTPRequestHandler, HTTPServer

COMPLIMENTS = [
    "Your queue names are impeccably chosen.",
    "You write remarkably readable code.",
    "A genuine pleasure to share a mesh with.",
    "Your task expiry values show real foresight.",
    "The kind of service that makes monitoring a joy.",
]


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if not self.path.startswith("/compliment"):
            self.send_response(404)
            self.end_headers()
            return
        body = json.dumps({"compliment": random.choice(COMPLIMENTS)}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        print(f"[svc-c] {fmt % args}", flush=True)


if __name__ == "__main__":
    HTTPServer(("", 8000), Handler).serve_forever()
