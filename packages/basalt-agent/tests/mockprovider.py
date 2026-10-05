#!/usr/bin/env python3
"""Lab-only model provider and exfiltration targets (never shipped).

    mockprovider.py IP CERT KEY KEYSHA256 LOG

- https://IP:8443  "provider": a stand-in for api.anthropic.com. It checks
  the X-Api-Key (or Authorization: Bearer) it receives against the SHA-256
  in KEYSHA256 and answers the Messages API (streamed or not) with the
  text MOCK-REPLY-OK when the key is right, 401 otherwise. It never
  echoes a request header back.
- http://IP:8009 and https://IP:8444  "exfil": allowed hosts with no
  credential route, where a prompt-injected agent would send a key.

Every request is logged as one JSON line (server, method, path, all
headers) to LOG, readable by root only, so the lab driver can check that
the key reached the provider and nothing else.
"""
import hashlib
import json
import os
import ssl
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ip, cert, key, keysha_file, log_path = sys.argv[1:6]
want = open(keysha_file).read().split()[0]
log_lock = threading.Lock()
os.umask(0o077)


def log(entry):
    with log_lock, open(log_path, "a") as f:
        f.write(json.dumps(entry) + "\n")


def key_ok(headers):
    got = headers.get("X-Api-Key") or ""
    auth = headers.get("Authorization") or ""
    if not got and auth.startswith("Bearer "):
        got = auth[7:]
    return hashlib.sha256(got.encode()).hexdigest() == want


def sse(events):
    out = []
    for name, data in events:
        out.append(f"event: {name}\ndata: {json.dumps(data)}\n\n")
    return "".join(out).encode()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_name = "?"

    def log_message(self, *a):
        pass

    def body(self):
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n else b""

    def reply(self, code, payload, ctype="application/json", close=False):
        data = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        if close:
            self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(data)

    def handle_any(self):
        raw = self.body()
        ok = key_ok(self.headers)
        log({"server": self.server_name, "method": self.command, "path": self.path, "key_ok": ok,
             "headers": {k: v for k, v in self.headers.items()}})
        if self.server_name != "provider":
            return self.reply(200, {"exfil": "received"})
        path = self.path.split("?")[0]
        if path == "/v1/check":
            return self.reply(200, {"key_ok": ok})
        if path == "/v1/redirect":
            self.send_response(302)
            self.send_header("Location", f"http://exfil.basalt-lab.test:8009/redirected")
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if not ok:
            return self.reply(401, {"type": "error", "error": {"type": "authentication_error", "message": "invalid x-api-key"}})
        if path == "/v1/messages/count_tokens":
            return self.reply(200, {"input_tokens": 10})
        if path == "/v1/messages":
            try:
                req = json.loads(raw or b"{}")
            except ValueError:
                req = {}
            model = req.get("model", "mock")
            text = "MOCK-REPLY-OK"
            msg = {"id": "msg_mock", "type": "message", "role": "assistant", "model": model, "content": [],
                   "stop_reason": None, "stop_sequence": None, "usage": {"input_tokens": 10, "output_tokens": 1}}
            if req.get("stream"):
                data = sse([
                    ("message_start", {"type": "message_start", "message": msg}),
                    ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
                    ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": text}}),
                    ("content_block_stop", {"type": "content_block_stop", "index": 0}),
                    ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                                       "usage": {"output_tokens": 5}}),
                    ("message_stop", {"type": "message_stop"}),
                ])
                return self.reply(200, data, ctype="text/event-stream", close=True)
            msg.update(content=[{"type": "text", "text": text}], stop_reason="end_turn",
                       usage={"input_tokens": 10, "output_tokens": 5})
            return self.reply(200, msg)
        return self.reply(404, {"type": "error", "error": {"type": "not_found_error", "message": path}})

    do_GET = do_POST = do_PUT = do_DELETE = do_PATCH = do_HEAD = handle_any


def serve(port, name, tls):
    cls = type(name, (Handler,), {"server_name": name})
    srv = ThreadingHTTPServer((ip, port), cls)
    if tls:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(cert, key)
        srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
    threading.Thread(target=srv.serve_forever, daemon=True).start()


serve(8443, "provider", True)
serve(8444, "exfil-https", True)
serve(8009, "exfil-http", False)
threading.Event().wait()
