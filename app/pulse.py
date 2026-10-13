import asyncio
import io
import json
import re
import secrets
import time
from dataclasses import dataclass
from urllib.parse import urlparse
import httpx
import qrcode
from pathlib import Path
from . import protocol
from .vault import Conflict, digest

MAX_RESPONSE = 1024 * 1024
COOKIE_NAME = re.compile(r"^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

def cookie_header(cookies):
    result = []
    for cookie in cookies:
        name, value = cookie['name'], cookie['value']
        if not value or name.startswith('.AspNetCore.Correlation.') or name.startswith('.AspNetCore.OpenIdConnect.Nonce.'):
            continue
        if not COOKIE_NAME.fullmatch(name) or any(ord(c) < 33 or ord(c) > 126 or c in '\",;\\' for c in value):
            raise ValueError('Invalid cookie')
        result.append(f'{name}={value}')
    header = '; '.join(result)
    if not header or len(header) > 65536:
        raise ValueError('Missing cookie')
    return header

def validate_header(header):
    if not isinstance(header, str) or not header or len(header) > 65536:
        raise ValueError('Invalid session header')
    seen = set()
    for item in header.split(';'):
        name, sep, value = item.strip().partition('=')
        if not sep or name in seen or not COOKIE_NAME.fullmatch(name) or not value or any(ord(c) < 33 or ord(c) > 126 or c in '\",;\\' for c in value):
            raise ValueError('Invalid session header')
        seen.add(name)
    return header

@dataclass
class Reply:
    status: int
    headers: dict
    body: bytes

class PulseTransport:
    def __init__(self):
        self.http = httpx.AsyncClient(timeout=15, follow_redirects=False)

    async def rpc(self, cookie, method, body, authorization=None):
        headers = {'content-type': 'application/grpc-web+proto', 'accept': 'application/grpc-web+proto',
          'x-grpc-web': '1', 'x-requested-with': 'XMLHttpRequest', 'origin': protocol.HOST,
          'referer': protocol.HOST + '/services', 'cookie': validate_header(cookie)}
        if authorization:
            headers['authorization'] = 'Bearer ' + authorization
        async with self.http.stream('POST', protocol.HOST + protocol.METHODS[method], content=body, headers=headers) as response:
            data = bytearray()
            async for chunk in response.aiter_bytes():
                if len(data) + len(chunk) > MAX_RESPONSE:
                    raise ValueError('Upstream response exceeds limit')
                data.extend(chunk)
            return Reply(response.status_code, dict(response.headers), bytes(data))

    async def identity(self, cookie):
        try:
            reply = await self.rpc(cookie, 'profile', protocol.profile_request())
            return protocol.authentication_state(reply.status, reply.headers, reply.body)
        except (httpx.HTTPError, ValueError, OSError):
            return 'unavailable', None

    async def close(self):
        await self.http.aclose()

@dataclass
class LoginJob:
    id: str
    capability_hash: str
    label: str
    account_id: str | None
    created: float
    expected_version: int | None = None
    status: str = 'waiting'
    qr: str | None = None
    error: str | None = None
    result_id: str | None = None
    task: object = None

class BrowserLogins:
    def __init__(self, vault, changed):
        self.vault, self.changed = vault, changed
        self.jobs = {}
        self.playwright = None
        self.browser = None
        self.start_lock = asyncio.Lock()
        self.slots = asyncio.Semaphore(2)
        self.script = Path(__file__).with_name('mirea_qr.js').read_text()

    async def _browser(self):
        async with self.start_lock:
            if self.browser is None or not self.browser.is_connected():
                from playwright.async_api import async_playwright
                if self.playwright is None:
                    self.playwright = await async_playwright().start()
                self.browser = await self.playwright.chromium.launch(headless=True, args=['--disable-dev-shm-usage'])
            return self.browser

    async def _context(self, state=None):
        browser = await self._browser()
        context = await browser.new_context(storage_state=state, viewport={'width': 900, 'height': 900})
        async def guard(route):
            parsed = urlparse(route.request.url)
            # No caller-controlled URL is ever navigated or proxied. Prevent
            # browser resources from accessing arbitrary hosts or local services.
            allowed = parsed.scheme == 'https' and (parsed.hostname in {'pulse.mirea.ru', 'sso.mirea.ru'} or
              (parsed.hostname or '').endswith('.ddos-guard.net') or parsed.hostname == 'ddos-guard.net')
            if allowed:
                await route.continue_()
            else:
                await route.abort()
        await context.route('**/*', guard)
        return context

    async def capture(self, context):
        response = await context.request.post(protocol.HOST + protocol.PROFILE_PATH,
            data=protocol.profile_request(), headers={'content-type': 'application/grpc-web+proto', 'x-grpc-web': '1',
              'x-requested-with': 'XMLHttpRequest', 'origin': protocol.HOST}, timeout=15000, max_redirects=0)
        body = await response.body()
        if len(body) > MAX_RESPONSE:
            return None
        state, uid = protocol.authentication_state(response.status, response.headers, body)
        if state != 'valid':
            return None
        cookies = await context.cookies([protocol.HOST + protocol.PROFILE_PATH])
        header = cookie_header(cookies)
        # Only cookies with a known authentication-expiry meaning contribute.
        expiry = [c['expires'] for c in cookies if re.fullmatch(r'\.AspNetCore\.Cookies(?:C[1-9][0-9]*)?', c['name']) and c.get('expires', -1) > time.time()]
        return uid, {'cookie': header, 'storage_state': await context.storage_state(), 'expires_at': min(expiry) if expiry else None}

    def start(self, label, account_id=None):
        for job_id, job in list(self.jobs.items()):
            if time.time() - job.created > 900:
                if job.task and not job.task.done():
                    job.task.cancel()
                del self.jobs[job_id]
        if sum(j.status in ('waiting', 'login') for j in self.jobs.values()) >= 20:
            raise ValueError('Too many active logins')
        capability = secrets.token_urlsafe(32)
        existing = self.vault.account(account_id) if account_id else None
        job = LoginJob(secrets.token_urlsafe(18), digest(capability), label, account_id, time.time(), expected_version=existing['session_version'] if existing else None)
        self.jobs[job.id] = job
        job.task = asyncio.create_task(self._login(job))
        return job, capability

    async def _login(self, job):
        context = None
        try:
            async with self.slots:
                if time.time() - job.created > 600:
                    job.status = 'expired'
                    return
                context = await self._context()
                page = await context.new_page()
                job.status = 'login'
                await page.goto(protocol.LOGIN_URL, wait_until='domcontentloaded', timeout=45000)
                while time.time() - job.created < 600:
                    if urlparse(page.url).hostname == 'pulse.mirea.ru':
                        captured = await self.capture(context)
                        if captured:
                            uid, credentials = captured
                            account = self.vault.upsert(uid, job.label, credentials, account_id=job.account_id, session_expected=job.expected_version)
                            job.result_id, job.status, job.qr = account['id'], 'complete', None
                            await self.changed()
                            return
                    elif urlparse(page.url).hostname == 'sso.mirea.ru':
                        try:
                            raw = await page.evaluate(self.script)
                            qr = json.loads(raw).get('qr') if isinstance(raw, str) else None
                            if qr is None:
                                alternative = page.locator('a, button').filter(has_text=re.compile(r'QR', re.IGNORECASE)).first
                                if await alternative.count() and await alternative.is_visible():
                                    await alternative.click(timeout=3000)
                            parsed = urlparse(qr or '')
                            if parsed.scheme == 'https' and parsed.hostname == 'sso.mirea.ru' and parsed.path == '/realms/mirea/qr-code-auth/scan':
                                job.qr = qr
                        except Exception:
                            # A successful QR submit destroys the previous JS context.
                            pass
                    await asyncio.sleep(2)
                job.status, job.qr = 'expired', None
        except Conflict:
            job.status, job.error = 'failed', 'The login belongs to a different account or a newer version was stored. Start again.'
        except asyncio.CancelledError:
            job.status, job.qr = 'cancelled', None
            raise
        except Exception:
            # Never include browser URLs, cookies or exception contents in logs.
            job.status, job.error = 'failed', 'Could not complete Pulse login. Retry in the panel.'
        finally:
            if context:
                await context.close()

    async def refresh(self, account):
        secret = self.vault.secret(account)
        if not secret.get('storage_state'):
            return False
        context = None
        try:
            async with self.slots:
                context = await self._context(secret['storage_state'])
                page = await context.new_page()
                await page.goto(protocol.LOGIN_URL, wait_until='domcontentloaded', timeout=30000)
                for _ in range(8):
                    if urlparse(page.url).hostname == 'pulse.mirea.ru':
                        captured = await self.capture(context)
                        if captured:
                            uid, credentials = captured
                            self.vault.upsert(uid, account['label'], credentials, account_id=account['id'], session_expected=account['session_version'])
                            await self.changed()
                            return True
                    await asyncio.sleep(1)
        except asyncio.CancelledError:
            raise
        except Exception:
            return False
        finally:
            if context:
                await context.close()
        return False

    def qr_png(self, job):
        if not job.qr:
            return None
        image = qrcode.make(job.qr)
        output = io.BytesIO()
        image.save(output, format='PNG')
        return output.getvalue()

    async def close(self):
        tasks = [job.task for job in self.jobs.values() if job.task and not job.task.done()]
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        if self.browser:
            await self.browser.close()
        if self.playwright:
            await self.playwright.stop()
