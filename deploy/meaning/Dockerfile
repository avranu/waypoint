# meaning: Waypoint's small model service on the compose network only (no
# published port). It judges how texts relate in meaning: e5 embeddings
# (/v1/embeddings) and a DeBERTa NLI cross-encoder (/v1/nli), on CPU. Both
# models are downloaded here, pinned to a revision, and the running container
# makes no outside calls (HF_HUB_OFFLINE=1). API: docs/DEPLOY.md.
# requirements.txt pins every dependency with hashes, resolved from
# requirements.in with `uv pip compile --exclude-newer <7 days before>` so
# each version is at least 7 days old (WP-844).
FROM python:3.12-slim
ENV PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 HF_HOME=/opt/hf
WORKDIR /app
COPY requirements.txt fetch_models.py ./
RUN pip install --require-hashes --no-deps \
      --index-url https://pypi.org/simple --extra-index-url https://download.pytorch.org/whl/cpu \
      -r requirements.txt \
 && python fetch_models.py \
 && useradd --create-home --uid 10001 meaning \
 && chmod -R a+rX /opt/hf
COPY server.py ./
ENV HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1 TOKENIZERS_PARALLELISM=false MEANING_THREADS=4
USER meaning
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s \
  CMD ["python", "-c", "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8080/health', timeout=4).status==200 else 1)"]
ENTRYPOINT ["python", "/app/server.py"]
