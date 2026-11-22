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
# /health runs both models on a word (or says they are busy, not stuck), so a
# process that answers HTTP with its models wedged turns unhealthy; a batch
# stuck past MEANING_STUCK_S (180) ends the process and compose restarts it.
HEALTHCHECK --interval=30s --timeout=10s --start-period=120s --retries=3 \
  CMD ["python", "-c", "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8080/health', timeout=9).status==200 else 1)"]
ENTRYPOINT ["python", "/app/server.py"]
