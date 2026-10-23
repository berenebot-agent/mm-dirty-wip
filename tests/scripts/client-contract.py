#!/usr/bin/env python3
"""Deterministic client contract check; needs Python 3, bash, curl and jq."""

import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
import subprocess
import threading
from urllib.parse import parse_qs, urlsplit


def main():
    requests = []
    sends = []

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            sends.append((urlsplit(self.path), body, self.headers["Idempotency-Key"]))
            response = json.dumps({"queued": True, "messageId": "<test>",
                                   "provider_message_id": "", "message": {"id": "msg_test"}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(response)))
            self.end_headers()
            self.wfile.write(response)

        def do_GET(self):
            assert self.headers["Authorization"] == "Bearer test-key"
            requests.append(urlsplit(self.path))
            body = json.dumps({"items": [], "completeness": "complete"}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *_):
            pass

    root = Path(__file__).resolve().parents[2]
    assets = root / "internal/httpapp/assets"
    server = HTTPServer(("127.0.0.1", 0), Handler)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    try:
        for executable, filename in [("python3", "mailmoose.py"), ("bash", "mailmoose.sh")]:
            command = [executable, str(assets / filename), "--base",
                       f"http://127.0.0.1:{server.server_port}", "--key", "test-key",
                       "--inbox", "inb_test"]
            for resource in ["list", "threads", "search", "labels"]:
                args = [resource]
                if resource != "labels":
                    args += ["--before", "opaque-continuation", "--folder", "Projects"]
                if resource == "search":
                    args += ["--subject", "invoice"]  # filter-only search
                result = subprocess.run(command + args, capture_output=True, text=True, timeout=10)
                assert result.returncode == 0, (filename, args, result.stderr)
                json.loads(result.stdout)
                request = requests.pop()
                expected = {"list": "/v1/messages", "threads": "/v1/threads",
                            "search": "/v1/search", "labels": "/v1/inboxes/inb_test/label-catalog"}
                assert request.path == expected[resource], request
                if resource != "labels":
                    query = parse_qs(request.query)
                    assert query["before"] == ["opaque-continuation"], query
                    assert query["folder"] == ["Projects"], query
                    assert query["inbox"] == ["inb_test"], query
                    assert "cursor" not in query, query
            for action in ["send", "reply", "forward"]:
                args = [action] + ([] if action == "send" else ["msg_source"])
                args += ["--to", "friend@example.test", "--cc", "cc@example.test",
                         "--bcc", "bcc@example.test", "--subject", "Custom",
                         "--text", "note", "--sender", "alias@example.test",
                         "--wait", "--idempotency-key", "test-idem"]
                result = subprocess.run(command + args, capture_output=True, text=True, timeout=10)
                assert result.returncode == 0, (filename, args, result.stderr)
                assert json.loads(result.stdout)["queued"] is True
                path, body, idem = sends.pop()
                assert path.path == ("/v1/send" if action == "send" else f"/v1/messages/msg_source/{action}")
                assert parse_qs(path.query) == {"wait": ["true"]}
                assert body == {"to": ["friend@example.test"], "cc": ["cc@example.test"],
                                "bcc": ["bcc@example.test"], "subject": "Custom", "text": "note",
                                "sender": "alias@example.test", **({"inbox_id": "inb_test"} if action == "send" else {})}, body
                assert idem == "test-idem", idem
    finally:
        server.shutdown()
        worker.join()
        server.server_close()
    print("Python and Bash onboarding client contracts passed")


if __name__ == "__main__":
    main()
