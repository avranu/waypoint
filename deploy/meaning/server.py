"""meaning: Waypoint's small model service. Embeddings (e5) and NLI (DeBERTa).

JSON over HTTP, no outside calls: both models are baked into the image and
loaded with HF_HUB_OFFLINE=1. See docs/DEPLOY.md and deploy/meaning/Dockerfile.
"""

import hmac
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import torch

MAX_ITEMS = 256
MAX_CHARS = 2000
BATCH = 32
MAX_BODY = 4 * 1024 * 1024
MAX_TOKENS = 512

# id -> settings. prefix is added to every text by the service (e5 needs it).
MODELS = {
    "intfloat/e5-base-v2": {
        "kind": "embed",
        "revision": "f52bf8ec8c7124536f0efb74aca902b2995e5bcd",
        "prefix": "query: ",
    },
    "cross-encoder/nli-deberta-v3-base": {
        "kind": "nli",
        "revision": "6c749ce3425cd33b46d187e45b92bbf96ee12ec7",
        "prefix": "",
    },
}
DEFAULTS = {"embed": "intfloat/e5-base-v2", "nli": "cross-encoder/nli-deberta-v3-base"}

TOKEN = os.environ.get("MEANING_TOKEN", "")
_lock = threading.Lock()  # one forward pass at a time; threads are for I/O
_loaded = {}


class BadRequest(Exception):
    pass


def load_all():
    from sentence_transformers import SentenceTransformer
    from transformers import AutoModelForSequenceClassification, AutoTokenizer

    torch.manual_seed(0)
    torch.set_num_threads(max(1, int(os.environ.get("MEANING_THREADS", "4") or 4)))
    for mid, m in MODELS.items():
        if m["kind"] == "embed":
            model = SentenceTransformer(mid, revision=m["revision"], device="cpu")
            model.max_seq_length = MAX_TOKENS
            model.eval()
            _loaded[mid] = model
        else:
            tok = AutoTokenizer.from_pretrained(mid, revision=m["revision"])
            model = AutoModelForSequenceClassification.from_pretrained(mid, revision=m["revision"])
            model.eval()
            labels = {int(i): str(v).lower() for i, v in model.config.id2label.items()}
            for want in ("entailment", "neutral", "contradiction"):
                if want not in labels.values():
                    raise RuntimeError(f"{mid}: id2label {labels} has no {want}")
            _loaded[mid] = (tok, model, labels)


def clip(v, what):
    if not isinstance(v, str):
        raise BadRequest(f"{what} must be a string")
    return v[:MAX_CHARS]


def pick(body, kind):
    mid = body.get("model") or DEFAULTS[kind]
    m = MODELS.get(mid) if isinstance(mid, str) else None
    if m is None or m["kind"] != kind:
        raise BadRequest(f"unknown {kind} model {mid!r}")
    return mid, m, f"{mid}@{m['revision']}"


def embed(body):
    mid, m, tag = pick(body, "embed")
    texts = body.get("input")
    if isinstance(texts, str):
        texts = [texts]
    if not isinstance(texts, list) or not texts:
        raise BadRequest("input must be a non-empty list of strings")
    if len(texts) > MAX_ITEMS:
        raise BadRequest(f"at most {MAX_ITEMS} inputs per request")
    texts = [m["prefix"] + clip(t, "input item") for t in texts]
    out = []
    with _lock, torch.inference_mode():
        for i in range(0, len(texts), BATCH):
            vecs = _loaded[mid].encode(
                texts[i : i + BATCH],
                batch_size=BATCH,
                normalize_embeddings=True,
                convert_to_numpy=True,
                show_progress_bar=False,
            )
            out.extend(vecs.tolist())
    return {
        "object": "list",
        "model": tag,
        "data": [{"object": "embedding", "index": i, "embedding": v} for i, v in enumerate(out)],
    }


def nli(body):
    mid, m, tag = pick(body, "nli")
    pairs = body.get("pairs")
    if not isinstance(pairs, list) or not pairs:
        raise BadRequest("pairs must be a non-empty list of {premise, hypothesis}")
    if len(pairs) > MAX_ITEMS:
        raise BadRequest(f"at most {MAX_ITEMS} pairs per request")
    prem, hyp = [], []
    for p in pairs:
        if not isinstance(p, dict):
            raise BadRequest("each pair must be an object with premise and hypothesis")
        prem.append(clip(p.get("premise"), "premise"))
        hyp.append(clip(p.get("hypothesis"), "hypothesis"))
    tok, model, labels = _loaded[mid]
    scores = []
    with _lock, torch.inference_mode():
        for i in range(0, len(pairs), BATCH):
            enc = tok(
                prem[i : i + BATCH],
                hyp[i : i + BATCH],
                padding=True,
                truncation=True,
                max_length=MAX_TOKENS,
                return_tensors="pt",
            )
            probs = torch.softmax(model(**enc).logits.float(), dim=-1).tolist()
            for row in probs:
                scores.append({labels[j]: row[j] for j in range(len(row))})
    return {"model": tag, "scores": scores}


def models():
    return {"data": [{"id": mid, "kind": m["kind"], "revision": m["revision"]} for mid, m in MODELS.items()]}


class Handler(BaseHTTPRequestHandler):
    timeout = 30  # a client that stalls mid-request frees its thread
    protocol_version = "HTTP/1.1"
    server_version = "meaning"

    def log_message(self, fmt, *args):
        sys.stderr.write("%s %s\n" % (self.address_string(), fmt % args))

    def send(self, code, obj):
        data = json.dumps(obj, separators=(",", ":")).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def err(self, code, msg):
        self.send(code, {"error": {"message": msg}})

    def authed(self):
        if not TOKEN:
            return True
        got = self.headers.get("Authorization", "")
        return hmac.compare_digest(got.encode(), f"Bearer {TOKEN}".encode())

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/health":
            return self.send(200, {"ok": True})
        if not self.authed():
            return self.err(401, "missing or wrong bearer token")
        if path == "/v1/models":
            return self.send(200, models())
        self.err(404, "not found")

    def do_POST(self):
        path = self.path.split("?")[0]
        try:
            length = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            length = -1
        # The body is not read on these early errors, so close the connection.
        if not self.authed():
            self.close_connection = True
            return self.err(401, "missing or wrong bearer token")
        fn = {"/v1/embeddings": embed, "/v1/nli": nli}.get(path)
        if fn is None:
            self.close_connection = True
            return self.err(404, "not found")
        if length < 0 or length > MAX_BODY:
            self.close_connection = True
            return self.err(400, "bad or too large request body")
        try:
            body = json.loads(self.rfile.read(length) or b"null")
            if not isinstance(body, dict):
                raise BadRequest("body must be a JSON object")
            self.send(200, fn(body))
        except (BadRequest, ValueError) as e:
            self.err(400, str(e))
        except Exception as e:  # keep serving
            sys.stderr.write(f"error: {e!r}\n")
            self.err(500, "internal error")


def main():
    load_all()
    srv = ThreadingHTTPServer(("0.0.0.0", 8080), Handler)
    srv.daemon_threads = True
    print("meaning: ready", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
