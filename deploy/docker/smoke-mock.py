#!/usr/bin/env python3

import argparse
import datetime
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


TENANT_KEY = "srt_0123456789abcdef0123456789abcdef"
UPSTREAM_TOKEN = "sk-docker-upstream-token"


class Handler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:
        if self.path == "/health":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
            return
        self.send_error(404)

    def do_POST(self) -> None:
        length = int(self.headers.get("Content-Length", "0"))
        if length:
            self.rfile.read(length)

        if self.path == f"/t/{TENANT_KEY}/_subrouter/leases":
            now = datetime.datetime.now(datetime.timezone.utc)
            self._json(
                {
                    "teamId": "docker-smoke-team",
                    "lease": {
                        "leaseId": "lease_docker_smoke",
                        "accountId": "apikey:docker",
                        "provider": "codex",
                        "authMode": "apikey",
                        "token": UPSTREAM_TOKEN,
                        "label": "apikey:docker",
                        "email": "apikey:docker",
                        "credentialGeneration": 1,
                        "issuedAt": now.isoformat().replace("+00:00", "Z"),
                        "expiresAt": (now + datetime.timedelta(minutes=5))
                        .isoformat()
                        .replace("+00:00", "Z"),
                    },
                }
            )
            return

        if self.path == f"/t/{TENANT_KEY}/_subrouter/leases/lease_docker_smoke/events":
            self.send_response(204)
            self.end_headers()
            return

        if self.path.endswith("/responses"):
            if self.headers.get("Authorization") != f"Bearer {UPSTREAM_TOKEN}":
                self.send_error(401)
                return
            self._json(
                {
                    "id": "resp_docker_smoke",
                    "object": "response",
                    "status": "completed",
                    "output": [],
                    "padding": "x" * (128 * 1024),
                }
            )
            return

        self.send_error(404)

    def _json(self, value: object) -> None:
        body = json.dumps(value).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, _format: str, *_args: object) -> None:
        return


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", required=True, type=int)
    args = parser.parse_args()
    ThreadingHTTPServer(("127.0.0.1", args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
