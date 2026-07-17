#!/usr/bin/env python3
"""Lightweight OpenAI-compatible embedding + rerank sidecar for workhorse-agent.

The memory subsystem's semantic retrieval speaks the OpenAI `/v1/embeddings`
protocol, and its optional rerank stage speaks the Cohere/Jina-compatible
`/v1/rerank` protocol (see docs/deployment.md → "Memory embedding"). This is a
minimal local endpoint backed by fastembed (ONNX, no torch, no CGO) so you get
both without bundling a model runtime into the Go binary.

    pip install fastembed
    EMBED_SERVER_MODEL=BAAI/bge-large-en-v1.5 \
    EMBED_SERVER_RERANK_MODEL=BAAI/bge-reranker-base \
    EMBED_SERVER_PORT=11434 python3 scripts/embed_server.py

Then in ~/.workhorse-agent/config.yaml:

    memory:
      embedding:
        base_url: http://127.0.0.1:11434/v1
        model:    BAAI/bge-large-en-v1.5
        rerank_model: BAAI/bge-reranker-base   # optional

Multilingual (Chinese + English): intfloat/multilingual-e5-large;
    rerank: jinaai/jina-reranker-v2-base-multilingual.
English-only:  BAAI/bge-large-en-v1.5 (1024-dim), BAAI/bge-base-en-v1.5 (768).
Leave EMBED_SERVER_RERANK_MODEL unset to skip loading a reranker.
Run `python3 scripts/embed_server.py --list` to see all supported models.
"""
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from fastembed import TextEmbedding

MODEL = os.environ.get("EMBED_SERVER_MODEL", "BAAI/bge-large-en-v1.5")
RERANK_MODEL = os.environ.get("EMBED_SERVER_RERANK_MODEL", "")
PORT = int(os.environ.get("EMBED_SERVER_PORT", "11434"))


def load_or_die(kind, loader, name, supported):
    names = {m["model"] for m in supported}
    if name not in names:
        print(f"error: {kind} model {name!r} is not supported by this fastembed version.", file=sys.stderr)
        print("supported models:", file=sys.stderr)
        for n in sorted(names):
            print(f"  {n}", file=sys.stderr)
        sys.exit(1)
    print(f"loading {kind} model {name} ...", flush=True)
    return loader(model_name=name)


def main():
    if "--list" in sys.argv:
        for m in TextEmbedding.list_supported_models():
            print(f"{m['model']:55s} dim={m.get('dim')} size={m.get('size_in_GB')}GB")
        try:
            from fastembed.rerank.cross_encoder import TextCrossEncoder
            print("--- rerank models ---")
            for m in TextCrossEncoder.list_supported_models():
                print(f"{m['model']:55s} size={m.get('size_in_GB')}GB")
        except ImportError:
            pass
        return

    embedder = load_or_die("embedding", TextEmbedding, MODEL, TextEmbedding.list_supported_models())
    reranker = None
    if RERANK_MODEL:
        from fastembed.rerank.cross_encoder import TextCrossEncoder
        reranker = load_or_die("rerank", TextCrossEncoder, RERANK_MODEL, TextCrossEncoder.list_supported_models())
    # Each ONNX session gets its own lock: sessions are not thread-safe
    # internally, but the embed and rerank models are independent sessions and
    # may run concurrently with each other.
    embed_lock = threading.Lock()
    rerank_lock = threading.Lock()
    dim = len(next(iter(embedder.embed(["warmup"]))))
    rr = RERANK_MODEL or "(disabled)"
    print(f"ready: embed={MODEL} dim={dim} rerank={rr}, listening on 127.0.0.1:{PORT}", flush=True)

    def embed_texts(texts):
        with embed_lock:
            return [v.tolist() for v in embedder.embed(texts)]

    def rerank_docs(query, docs):
        with rerank_lock:
            return [float(s) for s in reranker.rerank(query, docs)]

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
            path = self.path.rstrip("/")
            if path not in ("/v1/embeddings", "/embeddings", "/v1/rerank", "/rerank"):
                self._send(404, {"error": {"message": "not found"}})
                return
            try:
                n = int(self.headers.get("Content-Length", "0"))
                req = json.loads(self.rfile.read(n) or b"{}")
            except Exception as e:  # noqa: BLE001
                self._send(400, {"error": {"message": f"bad request: {e}"}})
                return
            if path.endswith("/rerank"):
                self._rerank(req)
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

        def _rerank(self, req):
            if reranker is None:
                self._send(503, {"error": {"message": "reranker not configured (set EMBED_SERVER_RERANK_MODEL)"}})
                return
            query = req.get("query", "")
            docs = req.get("documents", [])
            if not isinstance(query, str) or not query or not isinstance(docs, list) or not docs:
                self._send(400, {"error": {"message": "query (string) and documents (non-empty list) are required"}})
                return
            try:
                scores = rerank_docs(query, [str(d) for d in docs])
            except Exception as e:  # noqa: BLE001
                self._send(500, {"error": {"message": f"rerank failed: {e}"}})
                return
            order = sorted(range(len(scores)), key=lambda i: scores[i], reverse=True)
            top_n = req.get("top_n") or len(order)
            results = [{"index": i, "relevance_score": scores[i]} for i in order[:int(top_n)]]
            self._send(200, {"model": req.get("model", RERANK_MODEL), "results": results})

    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        sys.exit(0)


if __name__ == "__main__":
    main()
