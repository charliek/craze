"""A tiny status service: GET /health and GET /version, JSON bodies.

Runs in the foreground until interrupted. Listens on 127.0.0.1:$PORT (default 8000).
"""

import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer

VERSION = "2.7.3"
BUILD = "a41c9e0"

ROUTES = {
    "/health": {"status": "ok", "checks": {"db": "up", "queue": "up"}},
    "/version": {"version": VERSION, "build": BUILD},
}


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = ROUTES.get(self.path.split("?", 1)[0])
        if body is None:
            self.send_error(404, "no such route")
            return
        data = json.dumps(body, separators=(",", ":")).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, fmt, *args):
        print("%s - %s" % (self.address_string(), fmt % args), flush=True)


def main():
    port = int(os.environ.get("PORT", "8000"))
    server = HTTPServer(("127.0.0.1", port), Handler)
    print(f"Listening on http://127.0.0.1:{port}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
