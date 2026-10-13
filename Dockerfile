FROM python:3.12-slim-bookworm
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 DATA_DIR=/data PLAYWRIGHT_BROWSERS_PATH=/ms-playwright XDG_CACHE_HOME=/tmp/cache
WORKDIR /srv
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt && \
    playwright install --with-deps --only-shell chromium && \
    rm -rf /var/lib/apt/lists/* && \
    useradd --create-home --uid 10001 sessionserver && \
    mkdir -p /data && chown sessionserver:sessionserver /data
COPY --chown=sessionserver:sessionserver app ./app
COPY --chown=sessionserver:sessionserver static ./static
USER sessionserver
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/health', timeout=4)"
CMD ["uvicorn", "app.run:app", "--host", "0.0.0.0", "--port", "8080", "--workers", "1", "--no-access-log", "--no-proxy-headers"]
