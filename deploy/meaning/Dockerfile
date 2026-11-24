# requirements.txt pins every dependency with hashes, resolved so each version is at least 7 days old.
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
# /health runs both models on a word, so a process that answers HTTP with wedged models fails it.
HEALTHCHECK --interval=30s --timeout=10s --start-period=120s --retries=3 \
  CMD ["python", "-c", "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8080/health', timeout=9).status==200 else 1)"]
ENTRYPOINT ["python", "/app/server.py"]
