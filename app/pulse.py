import asyncio
import io
import json
import os
import re
import secrets
import time
from dataclasses import dataclass
from urllib.parse import urlparse, urlencode
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

    async def subscribed_group(self, cookie):
        # The account's own group, straight from Pulse: GetSubscribedSchedules takes
        # an empty request (identity comes from the cookie) and returns the group's
        # numeric schedule id and name — the real automatic group detection.
        try:
            reply = await self.rpc(cookie, 'schedules', protocol.frame(b''))
        except (httpx.HTTPError, ValueError, OSError):
            return None, None
        return protocol.subscribed_group(reply.body, reply.headers)

    async def logout(self, cookie):
        # Best-effort: end the SSO session on MIREA's side too, so the credentials
        # can't be reused anywhere. Failure here does not matter — the server clears
        # its own copy regardless.
        try:
            await self.http.get(protocol.HOST + '/api/auth/logout',
                params={'redirectUri': protocol.HOST}, headers={'cookie': validate_header(cookie)})
        except (httpx.HTTPError, ValueError, OSError):
            pass

    async def acs_timeline(self, cookie, uid, start, end):
        # Turnstile (ACS) entry/exit events over [start, end].
        try:
            reply = await self.rpc(cookie, 'acs-timeline', protocol.acs_timeline_request(uid, start, end))
        except (httpx.HTTPError, ValueError, OSError):
            return None
        return protocol.acs_events(reply.body, reply.headers)

    async def search_groups(self, query):
        # Resolve a group name to its numeric schedule id via the public MIREA
        # schedule API (schedule-of.mirea.ru) — the same source the official app
        # uses, so the id matches what the phone app expects. scheduleTarget 1 is
        # a group (2 teacher, 3 auditorium).
        try:
            response = await self.http.get('https://schedule-of.mirea.ru/schedule/api/search',
                params={'limit': '15', 'match': query}, headers={'accept': 'application/json'})
            if response.status_code != 200:
                return []
            payload = response.json()
        except (httpx.HTTPError, ValueError, OSError):
            return []
        items = payload.get('data') if isinstance(payload, dict) else None
        groups = []
        if isinstance(items, list):
            for item in items:
                if isinstance(item, dict) and item.get('scheduleTarget') == 1 and isinstance(item.get('id'), int):
                    groups.append({'id': item['id'], 'name': str(item.get('targetTitle') or ''),
                                   'full': str(item.get('fullTitle') or '')})
        return groups[:15]

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
    approval_submitted: bool = False
    approved_at: float | None = None

class BrowserLogins:
    def __init__(self, vault, changed):
        self.vault, self.changed = vault, changed
        self.jobs = {}
        self.playwright = None
        self.browser = None
        self.start_lock = asyncio.Lock()
        # Interactive logins and background renewals use SEPARATE browser budgets.
        # An interactive login holds its slot for the whole human QR wait, so with a
        # single shared pool a few people enrolling (or renewals firing as accounts
        # pile up) starved everyone else and logins timed out. Renewals get their own
        # small budget so they can never consume the interactive pool. Tune via env
        # if the host has little memory (each slot is one headless browser context).
        def _pos(name, default):
            try:
                return max(1, int(os.environ.get(name, str(default))))
            except (TypeError, ValueError):
                return default
        self.login_slots = asyncio.Semaphore(_pos('LOGIN_SLOTS', 6))
        self.renew_slots = asyncio.Semaphore(_pos('RENEW_SLOTS', 2))
        self.max_active = _pos('MAX_ACTIVE_LOGINS', 24)
        self.login_ttl = _pos('LOGIN_TTL_SECONDS', 600)
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
        return uid, {'cookie': header, 'storage_state': await context.storage_state(), 'expires_at': min(expiry) if expiry else None,
                     'renewed_at': time.time()}

    # Words on a post-approval "continue"/consent control that is safe to press,
    # and words that must never be pressed (they abort or sign the session out).
    ACCEPT = re.compile(r'продолж|подтверд|разреш|войт|вход|accept|allow|authoriz|continu|confirm|proceed|grant|submit|ок\b|ok\b', re.I)
    REJECT = re.compile(r'отмен|отклон|запрет|назад|cancel|deny|reject|decline|\bback\b|logout|выход|выйти', re.I)

    async def advance(self, page):
        # After the QR is approved, Pulse/SSO can land on an interstitial — an
        # OAuth consent screen or a plain "continue" page — whose button must be
        # pressed before the Pulse session cookie is issued. Press it ourselves so
        # the login completes without a human. The context route guard confines
        # every navigation to the Pulse/SSO/DDoS-Guard hosts, so a click here can
        # never leave those origins.
        try:
            controls = page.locator('button:visible, input[type=submit]:visible, input[type=button]:visible, a[role=button]:visible')
            count = min(await controls.count(), 25)
        except Exception:
            return
        fallback = None
        for index in range(count):
            control = controls.nth(index)
            try:
                label = ((await control.get_attribute('value')) or (await control.inner_text()) or (await control.get_attribute('aria-label')) or '').strip()
            except Exception:
                continue
            if self.REJECT.search(label):
                continue
            if self.ACCEPT.search(label):
                try:
                    await control.click(timeout=3000)
                except Exception:
                    pass
                return
            if fallback is None:
                fallback = control
        # No accept-labelled control. Only submit when the page offers a single
        # safe control, so an ambiguous multi-button page is left for a human.
        if fallback is not None and count == 1:
            try:
                await fallback.click(timeout=3000)
            except Exception:
                pass

    async def qr_step(self, context, page, job):
        data = await page.evaluate(self.script)
        if not isinstance(data, dict):
            return
        if data.get('error') or data.get('pageId') == 'error.ftl':
            raise ValueError('MIREA rejected the login; start a fresh QR login.')
        if job.approval_submitted:
            return
        qr = urlparse(data.get('qr') or '')
        if qr.scheme == 'https' and qr.hostname == 'sso.mirea.ru' and qr.path == '/realms/mirea/qr-code-auth/scan':
            job.qr = data['qr']
        else:
            alternative = page.locator('a, button').filter(has_text=re.compile(r'QR', re.IGNORECASE)).first
            if await alternative.count() and await alternative.is_visible():
                await alternative.click(timeout=3000)
            return
        if not data.get('sessionId') or not data.get('tabId'):
            return
        endpoint = 'https://sso.mirea.ru/realms/mirea/qr-code-auth/check-status?' + urlencode(
            {'sessionId': data['sessionId'], 'tabId': data['tabId']})
        response = await context.request.get(endpoint, timeout=15000, max_redirects=0)
        if response.status != 200:
            return
        body = await response.body()
        if len(body) > MAX_RESPONSE:
            return
        try:
            result = json.loads(body)
        except (ValueError, UnicodeError):
            return  # A gateway response must not consume a valid approval.
        if not isinstance(result, dict):
            return
        if result.get('status') in ('timeout', 'rejected', 'session_not_found'):
            raise ValueError('The QR approval expired or was rejected. Start a new login.')
        if result.get('authenticated') is not True:
            return
        action = urlparse(data.get('action') or '')
        if action.scheme != 'https' or action.hostname != 'sso.mirea.ru' or action.port not in (None, 443) or action.path != '/realms/mirea/login-actions/authenticate' or not data.get('execution'):
            raise ValueError('MIREA changed its QR completion form. Update the server.')
        # Only this worker submits approval. The theme's own polling is blocked
        # on this page, preventing two competing POSTs consuming the same login.
        job.qr = None
        job.approval_submitted = True
        job.approved_at = time.time()
        job.status = 'finishing'
        await page.evaluate('''data => {
          const form = document.createElement('form');
          form.method = 'post'; form.action = data.action;
          const input = document.createElement('input');
          input.type = 'hidden'; input.name = 'authenticationExecution'; input.value = data.execution;
          form.appendChild(input); document.body.appendChild(form);
          HTMLFormElement.prototype.submit.call(form);
        }''', {'action': data['action'], 'execution': data['execution']})

    def start(self, label, account_id=None):
        retain = max(900, self.login_ttl + 300)
        for job_id, job in list(self.jobs.items()):
            if time.time() - job.created > retain:
                if job.task and not job.task.done():
                    job.task.cancel()
                del self.jobs[job_id]
        if sum(j.status in ('waiting', 'login', 'finishing') for j in self.jobs.values()) >= self.max_active:
            raise ValueError('The server is busy with other logins right now; try again in a minute')
        capability = secrets.token_urlsafe(32)
        existing = self.vault.account(account_id) if account_id else None
        job = LoginJob(secrets.token_urlsafe(18), digest(capability), label, account_id, time.time(), expected_version=existing['session_version'] if existing else None)
        self.jobs[job.id] = job
        job.task = asyncio.create_task(self._login(job))
        return job, capability

    async def _login(self, job):
        context = None
        try:
            async with self.login_slots:
                if time.time() - job.created > self.login_ttl:
                    job.status = 'expired'
                    return
                context = await self._context()
                page = await context.new_page()
                await page.route('**/qr-code-auth/check-status?*', lambda route: route.abort())
                job.status = 'login'
                await page.goto(protocol.LOGIN_URL, wait_until='domcontentloaded', timeout=45000)
                while time.time() - job.created < self.login_ttl:
                    if job.approved_at and time.time() - job.approved_at > 90:
                        job.status, job.error = 'failed', 'MIREA approved the QR, but Pulse did not confirm the session. Start a new login.'
                        return
                    host = urlparse(page.url).hostname
                    if host == 'pulse.mirea.ru':
                        try:
                            captured = await self.capture(context)
                        except Exception:
                            captured = None  # Cookies/profile can lag behind the redirect.
                        if captured:
                            uid, credentials = captured
                            account = self.vault.upsert(uid, job.label, credentials, account_id=job.account_id, session_expected=job.expected_version)
                            job.result_id, job.status, job.qr = account['id'], 'complete', None
                            await self.changed()
                            return
                        # Not confirmed yet: clear any Pulse "continue"/consent page
                        # that is holding the OAuth callback open.
                        await self.advance(page)
                    elif host == 'sso.mirea.ru':
                        if job.approval_submitted:
                            # Authentication is already done; press through any
                            # post-login consent or required-action page SSO shows
                            # before it redirects back to Pulse.
                            await self.advance(page)
                        else:
                            try:
                                await self.qr_step(context, page, job)
                            except ValueError as error:
                                job.status, job.error, job.qr = 'failed', str(error), None
                                return
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
            async with self.renew_slots:
                context = await self._context(secret['storage_state'])
                page = await context.new_page()
                await page.goto(protocol.LOGIN_URL, wait_until='domcontentloaded', timeout=30000)
                for _ in range(10):
                    host = urlparse(page.url).hostname
                    if host == 'pulse.mirea.ru':
                        captured = await self.capture(context)
                        if captured:
                            uid, credentials = captured
                            self.vault.upsert(uid, account['label'], credentials, account_id=account['id'], session_expected=account['session_version'])
                            await self.changed()
                            return True
                        # A silent renewal can also hit the consent/continue page.
                        await self.advance(page)
                    elif host == 'sso.mirea.ru':
                        # Replaying the saved SSO state may surface a consent page
                        # but never a QR challenge, so only press a safe control.
                        await self.advance(page)
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
