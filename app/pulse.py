import asyncio
import base64
import logging
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

log = logging.getLogger('aa.login')

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

    async def _read(self, cookie, method, body, parse):
        try:
            reply = await self.rpc(cookie, method, body)
        except (httpx.HTTPError, ValueError, OSError):
            return None
        return parse(reply.body, reply.headers) if reply.status == 200 else None

    async def visiting_logs(self, cookie):
        return await self._read(cookie, 'visiting-logs', protocol.frame(b''), protocol.visiting_logs)

    async def day_lessons(self, cookie, day):
        return await self._read(cookie, 'day-lessons', protocol.day_lessons_request(day), protocol.day_lessons)

    async def lesson_attendance(self, cookie, lesson_id, log_id):
        return await self._read(cookie, 'lesson-attendance', protocol.lesson_attendance_request(lesson_id, log_id),
                                protocol.lesson_attendance)

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
    diag: str = ''

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

    async def _context(self, state=None, hosts=()):
        browser = await self._browser()
        context = await browser.new_context(storage_state=state, viewport={'width': 900, 'height': 900})
        # Playwright's default is 30s per action. advance() touches up to 25
        # controls, and during a redirect they detach, so each lookup waited the
        # full 30s and the login sat in "finishing" for many minutes.
        context.set_default_timeout(5000)
        context.set_default_navigation_timeout(30000)
        async def guard(route):
            parsed = urlparse(route.request.url)
            # No caller-controlled URL is ever navigated or proxied. Prevent
            # browser resources from accessing arbitrary hosts or local services.
            allowed = parsed.scheme == 'https' and (parsed.hostname in {'pulse.mirea.ru', 'sso.mirea.ru', *hosts} or
              (parsed.hostname or '').endswith('.ddos-guard.net') or parsed.hostname == 'ddos-guard.net')
            if allowed:
                await route.continue_()
            else:
                await route.abort()
        await context.route('**/*', guard)
        return context

    async def capture(self, context, job=None):
        response = await context.request.post(protocol.HOST + protocol.PROFILE_PATH,
            data=protocol.profile_request(), headers={'content-type': 'application/grpc-web+proto', 'x-grpc-web': '1',
              'x-requested-with': 'XMLHttpRequest', 'origin': protocol.HOST}, timeout=15000, max_redirects=0)
        body = await response.body()
        if len(body) > MAX_RESPONSE:
            return None
        state, uid = protocol.authentication_state(response.status, response.headers, body)
        if job is not None:
            job.diag = f'{response.status}/{state}'
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

    SKIP = re.compile(r'пропуст|не сейчас|позже|skip|later|not now', re.I)

    async def skip_required_action(self, page):
        # SSO can insert an optional required action (e.g. "link MAX") before it
        # returns to Pulse. Its buttons are icon-only or links, so advance() saw
        # nothing to press. Press only an explicit skip control, never "confirm",
        # which would start linking a messenger to the student's account.
        selectors = ['[name=cancel-aia]', '[name*=skip i]', '[id*=skip i]']
        for selector in selectors:
            target = page.locator(selector).first
            try:
                if await target.count():
                    await target.click(timeout=3000)
                    return True
            except Exception:
                pass
        target = page.locator('a, button, input[type=submit], input[type=button], [role=button]').filter(has_text=self.SKIP).first
        try:
            if await target.count():
                await target.click(timeout=3000)
                return True
        except Exception:
            pass
        for element in await page.locator('input[type=submit], input[type=button]').all():
            try:
                if self.SKIP.search(await element.get_attribute('value') or ''):
                    await element.click(timeout=3000)
                    return True
            except Exception:
                pass
        return False

    async def advance(self, page):
        if '/login-actions/required-action' in urlparse(getattr(page, 'url', '') or '').path:
            await self.skip_required_action(page)
            return
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

    async def safe_advance(self, page):
        try:
            await asyncio.wait_for(self.advance(page), 15)
        except asyncio.CancelledError:
            raise
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
                        where = urlparse(page.url)
                        try:
                            buttons = await asyncio.wait_for(page.locator('button:visible, input[type=submit]:visible, a[role=button]:visible').all_inner_texts(), 5)
                        except Exception:
                            buttons = []
                        diag = f'stuck at {where.hostname}{where.path}; profile={job.diag or "-"}; buttons={[b.strip()[:30] for b in buttons[:8]]}'
                        log.warning('login %s: %s', job.id[:6], diag)
                        job.status, job.error = 'failed', 'Pulse не подтвердил сессию (' + diag + '). Начните заново.'
                        return
                    host = urlparse(page.url).hostname
                    if host == 'pulse.mirea.ru':
                        try:
                            captured = await asyncio.wait_for(self.capture(context, job), 20)
                        except Exception as error:
                            job.diag = 'exc:' + type(error).__name__
                            captured = None  # Cookies/profile can lag behind the redirect.
                        if captured:
                            uid, credentials = captured
                            account = self.vault.upsert(uid, job.label, credentials, account_id=job.account_id, session_expected=job.expected_version)
                            job.result_id, job.status, job.qr = account['id'], 'complete', None
                            await self.changed()
                            return
                        # Not confirmed yet: clear any Pulse "continue"/consent page
                        # that is holding the OAuth callback open.
                        await self.safe_advance(page)
                    elif host == 'sso.mirea.ru':
                        if job.approval_submitted:
                            # Authentication is already done; press through any
                            # post-login consent or required-action page SSO shows
                            # before it redirects back to Pulse.
                            await self.safe_advance(page)
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
        except Conflict as error:
            log.warning('login %s: conflict: %s', job.id[:6], error)
            job.status, job.error = 'failed', 'The login belongs to a different account or a newer version was stored. Start again.'
        except asyncio.CancelledError:
            job.status, job.qr = 'cancelled', None
            raise
        except Exception as error:
            # Never include browser URLs, cookies or exception contents in logs.
            log.warning('login %s: %s', job.id[:6], type(error).__name__)
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

    async def record(self, account, seconds=8):
        """Open the Pulse web pages with the account's saved session and record every
        gRPC call they make, plus the gRPC methods named in their scripts. For the
        admin panel: it shows where Pulse keeps data the server does not read yet,
        such as lesson attendance. Nothing is sent but what the pages themselves send."""
        secret = self.vault.secret(account)
        if not secret.get('storage_state'):
            raise ValueError('У аккаунта нет сохранённого входа браузера')
        calls, methods, visited, pending = [], set(), [], []
        method_pattern = re.compile(r'/(?:rtu[_a-z]*|rtu_tc)\.[A-Za-z0-9_.]+/[A-Z][A-Za-z0-9]+')
        async def on_response(response):
            try:
                kind = response.headers.get('content-type', '')
                url = urlparse(response.url)
                if 'javascript' in kind:
                    text = await response.text()
                    methods.update(method_pattern.findall(text[:8_000_000]))
                    return
                if 'grpc-web' not in kind:
                    return
                body = await response.body()
                if 'grpc-web-text' in kind:
                    body = base64.b64decode(body)
                headers = dict(response.headers)
                headers['content-type'] = 'application/grpc-web+proto'
                try:
                    message, status = protocol.grpc_message(body[:MAX_RESPONSE], headers)
                except ValueError as error:
                    message, status = None, str(error)
                request = response.request
                sent = request.post_data_buffer or b''
                if 'grpc-web-text' in (request.headers.get('content-type') or ''):
                    sent = base64.b64decode(sent)
                calls.append({'host': url.hostname, 'method': url.path, 'status': response.status, 'grpc_status': status,
                              'request': protocol.decode_tree(sent[5:]) if len(sent) > 5 else {},
                              'response': protocol.decode_tree(message) if message else {},
                              'response_bytes': len(message or b'')})
            except Exception as error:
                calls.append({'method': response.url, 'error': type(error).__name__})
        context = None
        async with self.renew_slots:
            try:
                context = await self._context(secret['storage_state'], hosts=('attendance-app.mirea.ru',))
                page = await context.new_page()
                page.on('response', lambda response: pending.append(asyncio.ensure_future(on_response(response))))
                queue = [protocol.HOST + '/services', 'https://attendance-app.mirea.ru/']
                while queue and len(visited) < 8:
                    url = queue.pop(0)
                    if url in visited:
                        continue
                    visited.append(url)
                    try:
                        await page.goto(url, wait_until='domcontentloaded', timeout=30000)
                        await asyncio.sleep(seconds)
                        links = await page.eval_on_selector_all('a[href]', 'nodes => nodes.map(n => [n.href, n.textContent || ""])')
                    except Exception as error:
                        log.warning('Pulse recording: %s failed: %s', url, type(error).__name__)
                        continue
                    for href, text in links:
                        host = urlparse(href).hostname
                        if host in ('pulse.mirea.ru', 'attendance-app.mirea.ru') and href not in visited and \
                                re.search(r'посещ|attend|журнал|visit|lesson|занят|отмет', href + ' ' + text, re.I):
                            queue.append(href)
                await asyncio.gather(*pending, return_exceptions=True)
            finally:
                if context:
                    await context.close()
        return {'visited': visited, 'calls': calls, 'methods': sorted(methods)}

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
