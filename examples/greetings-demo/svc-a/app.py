"""svc-a: frontend service. Accepts a name, delegates greeting to svc-b."""
import json
import os
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

# In a direct deployment this would call svc-b over k8s DNS. In the mesh,
# SVC_B is overridden in the pod manifest to point to the eqlink sender instead.
SVC_B = os.environ.get("SVC_B", "http://svc-b:8000")


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if not self.path.startswith("/greet"):
            self.send_response(404)
            self.end_headers()
            return
        params = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        name = params.get("name", ["World"])[0]

        url = f"{SVC_B}/greet?name={urllib.parse.quote(name)}"
        try:
            with urllib.request.urlopen(url) as resp:
                data = json.loads(resp.read())
        except urllib.error.URLError as e:
            data = {"error": str(e)}

        body = json.dumps(data, indent=2).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        print(f"[svc-a] {fmt % args}", flush=True)


if __name__ == "__main__":
    HTTPServer(("", 8000), Handler).serve_forever()
