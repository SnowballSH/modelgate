#!/usr/bin/env bash
set -Eeuo pipefail
umask 0077

port="${1:-8099}"
capture="${2:-$(mktemp -t hermes-wire)}"

command -v python3 >/dev/null || {
  printf 'python3 is required\n' >&2
  exit 1
}

printf 'base_url http://127.0.0.1:%s/v1  capture %s  (ctrl-c to stop)\n' "${port}" "${capture}" >&2

exec python3 - "${capture}" "${port}" <<'PY'
import http.server
import json
import signal
import sys
import time

capture_path, port = sys.argv[1], int(sys.argv[2])

REPORTED = (
    "model",
    "stream",
    "temperature",
    "top_p",
    "reasoning_effort",
    "max_tokens",
    "max_completion_tokens",
    "parallel_tool_calls",
)


def summarise(body):
    if not isinstance(body, dict):
        return "body is not a JSON object"
    parts = [f"{name}={json.dumps(body[name])}" for name in REPORTED if name in body]
    absent = [name for name in ("temperature", "top_p") if name not in body]
    if absent:
        parts.append("absent=" + ",".join(absent))
    parts.append(f"tools={len(body.get('tools') or [])}")
    parts.append(f"messages={len(body.get('messages') or [])}")
    return " ".join(parts)


def completion(model):
    return {
        "id": "chatcmpl-capture",
        "object": "chat.completion",
        "created": int(time.time()),
        "model": model,
        "choices": [
            {
                "index": 0,
                "message": {"role": "assistant", "content": "captured"},
                "finish_reason": "stop",
            }
        ],
        "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
    }


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    def do_GET(self):
        if self.path.rstrip("/").endswith("/v1/models"):
            self.respond_json(200, {"object": "list", "data": [{"id": "capture", "object": "model"}]})
        else:
            self.respond_json(404, {"error": {"message": "not found", "type": "invalid_request_error"}})

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        try:
            body = json.loads(raw)
        except ValueError:
            body = {"unparsed_body": raw.decode("utf-8", "replace")}
        record = {
            "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "path": self.path,
            "body": body,
        }
        with open(capture_path, "a", encoding="utf-8") as handle:
            handle.write(json.dumps(record) + "\n")
        print(summarise(body), flush=True)

        model = body.get("model", "capture") if isinstance(body, dict) else "capture"
        if isinstance(body, dict) and body.get("stream"):
            self.respond_stream(model)
        else:
            self.respond_json(200, completion(model))

    def respond_json(self, status, payload):
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def respond_stream(self, model):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True
        base = {
            "id": "chatcmpl-capture",
            "object": "chat.completion.chunk",
            "created": int(time.time()),
            "model": model,
        }
        for delta, finish in (({"role": "assistant", "content": "captured"}, None), ({}, "stop")):
            chunk = dict(base, choices=[{"index": 0, "delta": delta, "finish_reason": finish}])
            self.wfile.write(f"data: {json.dumps(chunk)}\n\n".encode())
        self.wfile.write(b"data: [DONE]\n\n")


signal.signal(signal.SIGINT, lambda *_: sys.exit(0))
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
try:
    http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
except (KeyboardInterrupt, SystemExit):
    pass
PY
