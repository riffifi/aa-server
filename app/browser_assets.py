"""Small shared cache for explicitly public Pulse browser assets, never RPCs."""
import re
import time
from collections import OrderedDict
from urllib.parse import urlparse


class BrowserAssets:
    def __init__(self, budget=64 * 1024 * 1024, max_item=32 * 1024 * 1024):
        self.budget, self.max_item = budget, max_item
        self.entries = OrderedDict()
        self.size = 0
        self.downloaded_bytes = self.saved_bytes = self.hits = self.misses = 0

    def stats(self):
        return {'entries': len(self.entries), 'bytes': self.size,
                'downloaded_bytes': self.downloaded_bytes, 'saved_bytes': self.saved_bytes,
                'hits': self.hits, 'misses': self.misses}

    async def route(self, route):
        request = route.request
        url = urlparse(request.url)
        # Keep authenticated documents, API responses and SSO outside this cache.
        static = (request.resource_type in {'script', 'stylesheet', 'font'}
                  and url.path.lower().endswith(('.js', '.css', '.woff', '.woff2', '.ttf')))
        framework = (url.path.startswith('/_framework/')
                     and url.path.lower().endswith(('.wasm', '.dll', '.dat', '.bin', '.json')))
        if request.method != 'GET' or url.hostname != 'pulse.mirea.ru' or not (static or framework):
            await route.continue_()
            return
        key = request.url
        cached = self.entries.pop(key, None)
        if cached:
            expires, headers, body = cached
            self.size -= len(body)
            if expires > time.monotonic():
                self.entries[key] = cached
                self.size += len(body)
                self.hits += 1
                self.saved_bytes += len(body)
                await route.fulfill(status=200, headers=headers, body=body)
                return
        self.misses += 1
        # Preserve the original response, including cookies, on every cache miss.
        response = await route.fetch(max_redirects=0)
        try:
            headers = {k.lower(): v for k, v in response.headers.items()}
            control = headers.get('cache-control', '').lower()
            age = re.search(r'(?:^|,)\s*max-age\s*=\s*(\d+)', control)
            eligible = (response.status == 200 and 'public' in control and age
                        and not any(flag in control for flag in ('private', 'no-store', 'no-cache'))
                        and 'set-cookie' not in headers
                        and headers.get('vary', '').lower().strip() in ('', 'accept-encoding'))
            if eligible:
                body = await response.body()
                self.downloaded_bytes += len(body)
                try:
                    ttl = min(max(0, int(age[1]) - int(headers.get('age', '0'))), 86400)
                except ValueError:
                    ttl = 0
                if ttl > 0 and len(body) <= min(self.max_item, self.budget):
                    # Playwright returns decoded bytes; don't replay wire encoding/length.
                    saved_headers = {k: v for k, v in headers.items()
                                     if k not in {'content-encoding', 'content-length', 'transfer-encoding', 'connection'}}
                    previous = self.entries.pop(key, None)
                    if previous:
                        self.size -= len(previous[2])
                    while self.entries and (self.size + len(body) > self.budget or len(self.entries) >= 128):
                        _, old = self.entries.popitem(last=False)
                        self.size -= len(old[2])
                    self.entries[key] = (time.monotonic() + ttl, saved_headers, body)
                    self.size += len(body)
            await route.fulfill(response=response)
        finally:
            await response.dispose()
