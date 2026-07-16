#!/usr/bin/env python3
"""Lightweight OpenAI-compatible /v1/embeddings sidecar for workhorse-agent.

The memory subsystem's semantic retrieval speaks the OpenAI `/v1/embeddings`
protocol (see docs/deployment.md → "Memory embedding"). This is a minimal local
endpoint backed by fastembed (ONNX, no torch, no CGO) so you get semantic
retrieval without bundling a model runtime into the Go binary.

    pip install fastembed
    EMBED_SERVER_MODEL=BAAI/bge-m3 EMBED_SERVER_PORT=11434 python3 scripts/embed_server.py

Then in ~/.workhorse-agent/config.yaml:

    memory:
      embedding:
        base_url: http://127.0.0.1:11434/v1
        model:    BAAI/bge-m3

Multilingual (Chinese + English): BAAI/bge-m3, intfloat/multilingual-e5-large.
English-only, faster:             BAAI/bge-base-en-v1.5 (768-dim).
Run `python3 scripts/embed_server.py --list` to see all supported models.
"""
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from fastembed import TextEmbedding

MODEL = os.environ.get("EMBED_SERVER_MODEL", "BAAI/bge-m3")
PORT = int(os.environ.get("EMBED_SERVER_PORT", "11434"))


def main():
    if "--list" in sys.argv:
        for m in TextEmbedding.list_supported_models():
            print(f"{m['model']:55s} dim={m.get('dim')} size={m.get('size_in_GB')}GB")
        return

    print(f"loading embedding model {MODEL} ...", flush=True)
    embedder = TextEmbedding(model_name=MODEL)
    lock = threading.Lock()
    dim = len(next(iter(embedder.embed(["warmup"]))))
    print(f"model ready: {MODEL} dim={dim}, listening on 127.0.0.1:{PORT}", flush=True)

    def embed_texts(texts):
        # fastembed's ONNX session is not guaranteed thread-safe; serialize it.
        with lock:
            return [v.tolist() for v in embedder.embed(texts)]

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *a):  # quiet
            pass

        def _send(self, code, obj):
            body = json.dumps(obj).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            if self.path.rstrip("/") in ("/v1/models", "/health", ""):
                self._send(200, {"status": "ok", "model": MODEL, "dim": dim})
            else:
                self._send(404, {"error": {"message": "not found"}})

        def do_POST(self):
            if self.path.rstrip("/") not in ("/v1/embeddings", "/embeddings"):
                self._send(404, {"error": {"message": "not found"}})
                return
            try:
                n = int(self.headers.get("Content-Length", "0"))
                req = json.loads(self.rfile.read(n) or b"{}")
            except Exception as e:  # noqa: BLE001
                self._send(400, {"error": {"message": f"bad request: {e}"}})
                return
            inp = req.get("input", [])
            if isinstance(inp, str):
                inp = [inp]
            if not isinstance(inp, list) or not inp:
                self._send(400, {"error": {"message": "input must be a non-empty string or list"}})
                return
            try:
                vecs = embed_texts([str(x) for x in inp])
            except Exception as e:  # noqa: BLE001
                self._send(500, {"error": {"message": f"embed failed: {e}"}})
                return
            data = [{"object": "embedding", "index": i, "embedding": v} for i, v in enumerate(vecs)]
            self._send(200, {
                "object": "list",
                "data": data,
                "model": req.get("model", MODEL),
                "usage": {"prompt_tokens": 0, "total_tokens": 0},
            })

    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        sys.exit(0)


if __name__ == "__main__":
    main()
