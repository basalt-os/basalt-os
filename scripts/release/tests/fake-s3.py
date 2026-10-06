#!/usr/bin/env python3
"""A minimal S3 endpoint for the upload.sh tests (path-style, one bucket).

    fake-s3.py STATE_DIR PORT_FILE

Serves ListObjectsV2 (paged by FAKE_S3_PAGE_SIZE keys, default 1000),
HeadObject, GetObject and PutObject on 127.0.0.1 at a free port, written to
PORT_FILE once it listens. Objects live in STATE_DIR/objects/<key>, their
ETag (MD5, or a multipart-style value from STATE_DIR/etags) and headers in
STATE_DIR/meta/<key>.json. Every request is appended to STATE_DIR/requests
("METHOD key"), so a test can check what was sent and in which order.
Signatures are not checked: this is a local test double, not a server.
"""
import hashlib
import json
import os
import sys
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from xml.sax.saxutils import escape

STATE = sys.argv[1]
PORT_FILE = sys.argv[2]
PAGE = int(os.environ.get("FAKE_S3_PAGE_SIZE", "1000"))
LOCK = threading.Lock()


def obj_path(key):
    return os.path.join(STATE, "objects", key)


def meta_path(key):
    return os.path.join(STATE, "meta", key + ".json")


def all_keys():
    root = os.path.join(STATE, "objects")
    out = []
    for d, _, files in os.walk(root):
        for f in files:
            out.append(os.path.relpath(os.path.join(d, f), root))
    return sorted(out)


def read_meta(key):
    try:
        with open(meta_path(key)) as f:
            return json.load(f)
    except FileNotFoundError:
        # An object seeded by the test without metadata: plain MD5 ETag.
        with open(obj_path(key), "rb") as f:
            return {"etag": hashlib.md5(f.read()).hexdigest(), "headers": {}}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def split(self):
        url = urllib.parse.urlsplit(self.path)
        parts = url.path.lstrip("/").split("/", 1)
        key = urllib.parse.unquote(parts[1]) if len(parts) > 1 else ""
        return parts[0], key, urllib.parse.parse_qs(url.query)

    def record(self, method, key):
        with LOCK, open(os.path.join(STATE, "requests"), "a") as f:
            f.write(f"{method} {key}\n")

    def reply(self, code, body=b"", headers=None):
        self.send_response(code)
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def not_found(self):
        body = b"<?xml version='1.0'?><Error><Code>NoSuchKey</Code></Error>"
        self.reply(404, body, {"Content-Type": "application/xml"})

    def object_headers(self, key):
        m = read_meta(key)
        h = {"ETag": '"%s"' % m["etag"], "Content-Type": "application/octet-stream"}
        h.update(m.get("headers", {}))
        return h

    def do_GET(self):
        bucket, key, q = self.split()
        if not key and q.get("list-type") == ["2"]:
            self.record("LIST", q.get("prefix", [""])[0])
            prefix = q.get("prefix", [""])[0]
            keys = [k for k in all_keys() if k.startswith(prefix)]
            start = int(q.get("continuation-token", ["0"])[0])
            page = keys[start : start + PAGE]
            more = start + PAGE < len(keys)
            items = []
            for k in page:
                m = read_meta(k)
                size = os.path.getsize(obj_path(k))
                items.append(
                    f"<Contents><Key>{escape(k)}</Key><ETag>&quot;{m['etag']}&quot;</ETag>"
                    f"<Size>{size}</Size><StorageClass>STANDARD</StorageClass></Contents>"
                )
            body = (
                '<?xml version="1.0" encoding="UTF-8"?>'
                '<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">'
                f"<Name>{bucket}</Name><Prefix>{escape(prefix)}</Prefix><KeyCount>{len(page)}</KeyCount>"
                f"<MaxKeys>{PAGE}</MaxKeys><IsTruncated>{'true' if more else 'false'}</IsTruncated>"
                + (f"<NextContinuationToken>{start + PAGE}</NextContinuationToken>" if more else "")
                + "".join(items)
                + "</ListBucketResult>"
            ).encode()
            self.reply(200, body, {"Content-Type": "application/xml"})
            return
        self.record("GET", key)
        if not os.path.isfile(obj_path(key)):
            self.not_found()
            return
        with open(obj_path(key), "rb") as f:
            self.reply(200, f.read(), self.object_headers(key))

    def do_HEAD(self):
        _, key, _ = self.split()
        self.record("HEAD", key)
        if not os.path.isfile(obj_path(key)):
            self.reply(404)
            return
        h = self.object_headers(key)
        self.send_response(200)
        for k, v in h.items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(os.path.getsize(obj_path(key))))
        self.end_headers()

    def do_PUT(self):
        _, key, _ = self.split()
        n = int(self.headers.get("Content-Length", "0"))
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            self.reply(501)
            return
        data = self.rfile.read(n)
        self.record("PUT", key)
        os.makedirs(os.path.dirname(obj_path(key)), exist_ok=True)
        os.makedirs(os.path.dirname(meta_path(key)), exist_ok=True)
        with open(obj_path(key), "wb") as f:
            f.write(data)
        etag = hashlib.md5(data).hexdigest()
        keep = {k: v for k, v in self.headers.items() if k.lower() in ("cache-control", "content-type") or k.lower().startswith("x-amz-meta-")}
        with open(meta_path(key), "w") as f:
            json.dump({"etag": etag, "headers": keep}, f)
        self.reply(200, b"", {"ETag": '"%s"' % etag})


srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
with open(PORT_FILE + ".tmp", "w") as f:
    f.write(str(srv.server_address[1]))
os.rename(PORT_FILE + ".tmp", PORT_FILE)
srv.serve_forever()
