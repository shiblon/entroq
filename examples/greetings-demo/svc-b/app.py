"""svc-b: greeter service. Fetches a compliment from svc-c, then greets by name."""
import json
import os
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

# In a direct deployment this would call svc-c over k8s DNS. In the mesh,
# SVC_C is overridden in the pod manifest to point to the eqlink sender instead.
SVC_C = os.environ.get("SVC_C", "http://svc-c:8000")


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if not self.path.startswith("/greet"):
            self.send_response(404)
            self.end_headers()
            return
        params = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        name = params.get("name", ["World"])[0]

        try:
            with urllib.request.urlopen(f"{SVC_C}/compliment") as resp:
                compliment = json.loads(resp.read())["compliment"]
        except urllib.error.URLError as e:
            compliment = f"(svc-c unreachable: {e})"

        body = json.dumps({"greeting": f"Hello, {name}!", "compliment": compliment}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        print(f"[svc-b] {fmt % args}", flush=True)


if __name__ == "__main__":
    HTTPServer(("", 8000), Handler).serve_forever()
