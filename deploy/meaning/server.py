"""meaning: Waypoint's small model service. Embeddings (e5) and NLI (DeBERTa).

JSON over HTTP, no outside calls: both models are baked into the image and
loaded with HF_HUB_OFFLINE=1. See docs/DEPLOY.md and deploy/meaning/Dockerfile.
"""

import hmac
import json
import os
import sys
import threading
import time
import urllib.request
from contextlib import contextmanager
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
# A batch that runs longer than this is stuck (a batch of 32 takes seconds):
# the watchdog ends the process so compose restarts it (WP-1602).
STUCK_S = max(30, int(os.environ.get("MEANING_STUCK_S", "180") or 180))
SELF_FAILS = 6  # the watchdog's checks of /health, 10s apart, that may fail in a row
HEALTH_EVERY = 10  # seconds a health pass through the models stands for
_lock = threading.Lock()  # one forward pass at a time; threads are for I/O
_health_ok = 0.0  # time.monotonic() of the last health pass that ran the models
_beat = None  # time.monotonic() when the batch running now started; None when idle
_loaded = {}


@contextmanager
def forward(wait=-1):
    """Holds the model lock for a forward pass; False when it can't be had in wait seconds."""
    global _beat
    if not (_lock.acquire(blocking=False) if wait == 0 else _lock.acquire(timeout=wait)):
        yield False
        return
    try:
        with torch.inference_mode():
            beat()
            yield True
    finally:
        _beat = None
        _lock.release()


def beat():
    global _beat
    _beat = time.monotonic()


def stuck_for():
    b = _beat
    return 0.0 if b is None else time.monotonic() - b


def die(why):
    sys.stderr.write(f"meaning: {why}; exiting so the container restarts\n")
    sys.stderr.flush()
    os._exit(70)


def watchdog():
    """Ends the process when a batch is stuck, or when its own /health has
    failed SELF_FAILS times in a row (a server that stopped answering, or
    models that fail outside a batch). Compose restarts it; Docker's
    unhealthy mark alone restarts nothing."""
    fails = 0
    while True:
        time.sleep(10)
        took = stuck_for()
        if took > STUCK_S:
            die(f"a batch has run {took:.0f}s (limit {STUCK_S}s)")
        try:
            with urllib.request.urlopen("http://127.0.0.1:8080/health", timeout=30) as res:
                fails = 0 if res.status == 200 else fails + 1
        except Exception as e:  # an error status, a refused connection, a timeout
            fails += 1
            sys.stderr.write(f"watchdog: /health: {e!r}\n")
        if fails >= SELF_FAILS:
            die(f"/health failed {fails} times in a row")


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
    with forward():
        for i in range(0, len(texts), BATCH):
            beat()
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
    with forward():
        for i in range(0, len(pairs), BATCH):
            beat()
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


def health():
    """A real forward pass through both models, unless one is running: a
    process that answers HTTP but whose models are wedged is not healthy.
    It never waits for the lock, and a pass stands for HEALTH_EVERY seconds,
    so callers of /health (it needs no token) can't crowd out real work."""
    global _health_ok
    took = stuck_for()
    if took > STUCK_S:
        return 503, {"ok": False, "error": f"a batch has run {took:.0f}s"}
    if time.monotonic() - _health_ok < HEALTH_EVERY:
        return 200, {"ok": True}
    with forward(wait=0) as got:
        if not got:
            return 200, {"ok": True, "busy": True}
        _loaded[DEFAULTS["embed"]].encode(["query: health"], show_progress_bar=False)
        tok, model, _ = _loaded[DEFAULTS["nli"]]
        model(**tok(["health"], ["health"], return_tensors="pt"))
        _health_ok = time.monotonic()
    return 200, {"ok": True}


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
            try:
                return self.send(*health())
            except Exception as e:  # report, keep serving
                sys.stderr.write(f"health: {e!r}\n")
                return self.err(503, "health check failed")
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
    threading.Thread(target=watchdog, name="watchdog", daemon=True).start()
    srv = ThreadingHTTPServer(("0.0.0.0", 8080), Handler)
    srv.daemon_threads = True
    print("meaning: ready", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
