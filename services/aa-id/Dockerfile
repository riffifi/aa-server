FROM python:3.12-slim-bookworm
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 DATA_DIR=/data
WORKDIR /srv
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt && \
    useradd --create-home --uid 10002 aaid && \
    mkdir -p /data && chown aaid:aaid /data
COPY --chown=aaid:aaid aaid ./aaid
USER aaid
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8090/health', timeout=4)"
CMD ["uvicorn", "aaid.run:app", "--host", "0.0.0.0", "--port", "8090", "--workers", "1", "--no-access-log", "--no-proxy-headers"]
