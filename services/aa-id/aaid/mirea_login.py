"""MIREA QR sign-in for the AA ID site.

A headless browser opens the Pulse login, shows the person the MIREA QR code,
and after approval asks Pulse who signed in. The browser context, with every
MIREA cookie, is closed right after; AA ID keeps only the Pulse person id and
group."""
import asyncio
import io
import json
import logging
import re
import secrets
import time
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import urlencode, urlparse
import qrcode
from . import pulse

LOGIN_URL = pulse.HOST + '/api/auth/login?redirectUri=https%3A%2F%2Fpulse.mirea.ru%2Fservices&rememberMe=False'
log = logging.getLogger('aaid.login')

@dataclass
class LoginJob:
    id: str
    token_hash: str
    created: float
    status: str = 'waiting'
    qr: str | None = None
    error: str | None = None
    approval_submitted: bool = False
    approved_at: float | None = None
    identity: tuple | None = None
    claimed: bool = False
    task: object = None

class MireaLogins:
    ACCEPT = re.compile(r'продолж|подтверд|разреш|войт|вход|accept|allow|authoriz|continu|confirm|proceed|grant|submit|ок\b|ok\b', re.I)
    REJECT = re.compile(r'отмен|отклон|запрет|назад|cancel|deny|reject|decline|\bback\b|logout|выход|выйти', re.I)
    SKIP = re.compile(r'пропуст|не сейчас|позже|skip|later|not now', re.I)

    def __init__(self, slots=4, max_active=16, ttl=600):
        self.jobs = {}
        self.playwright = self.browser = None
        self.start_lock = asyncio.Lock()
        self.slots = asyncio.Semaphore(slots)
        self.max_active, self.ttl = max_active, ttl
        self.script = Path(__file__).with_name('mirea_qr.js').read_text()

    def start(self, digest):
        for job_id, job in list(self.jobs.items()):
            if time.time() - job.created > self.ttl + 300:
                if job.task and not job.task.done():
                    job.task.cancel()
                del self.jobs[job_id]
        if sum(j.status in ('waiting', 'login', 'finishing') for j in self.jobs.values()) >= self.max_active:
            raise ValueError('Сейчас много входов одновременно, попробуйте через минуту.')
        token = secrets.token_urlsafe(32)
        job = LoginJob(secrets.token_urlsafe(18), digest(token), time.time())
        self.jobs[job.id] = job
        job.task = asyncio.create_task(self._login(job))
        return job, token

    def qr_png(self, job):
        if not job.qr:
            return None
        output = io.BytesIO()
        qrcode.make(job.qr).save(output, format='PNG')
        return output.getvalue()

    async def _browser(self):
        async with self.start_lock:
            if self.browser is None or not self.browser.is_connected():
                from playwright.async_api import async_playwright
                if self.playwright is None:
                    self.playwright = await async_playwright().start()
                self.browser = await self.playwright.chromium.launch(headless=True, args=['--disable-dev-shm-usage'])
            return self.browser

    async def _context(self):
        context = await (await self._browser()).new_context(viewport={'width': 900, 'height': 900})
        context.set_default_timeout(5000)
        context.set_default_navigation_timeout(30000)
        async def guard(route):
            host = urlparse(route.request.url).hostname or ''
            # The browser may only talk to MIREA and its DDoS protection.
            if urlparse(route.request.url).scheme == 'https' and (host in {'pulse.mirea.ru', 'sso.mirea.ru'} or host == 'ddos-guard.net' or host.endswith('.ddos-guard.net')):
                await route.continue_()
            else:
                await route.abort()
        await context.route('**/*', guard)
        return context

    async def _call(self, context, path, body):
        response = await context.request.post(pulse.HOST + path, data=body, timeout=15000, max_redirects=0,
            headers={'content-type': 'application/grpc-web+proto', 'x-grpc-web': '1', 'x-requested-with': 'XMLHttpRequest', 'origin': pulse.HOST})
        data = await response.body()
        if len(data) > pulse.MAX_RESPONSE:
            raise ValueError('Upstream response exceeds limit')
        return response.status, response.headers, data

    async def identify(self, context):
        status, headers, body = await self._call(context, pulse.PROFILE, pulse.profile_request())
        try:
            uid = pulse.profile_uid(status, headers, body)
        except (ValueError, UnicodeError):
            return None
        if uid is None:
            return None
        try:
            status, headers, body = await self._call(context, pulse.SCHEDULES, pulse.frame(b''))
            group = pulse.subscribed_group(headers, body) if status == 200 else (None, None)
        except Exception:
            group = (None, None)
        return uid, *group

    async def _click(self, page, selector_or_locator):
        try:
            target = page.locator(selector_or_locator).first if isinstance(selector_or_locator, str) else selector_or_locator
            if await target.count():
                await target.click(timeout=3000)
                return True
        except Exception:
            pass
        return False

    async def advance(self, page):
        # Press through MIREA's post-approval consent / "continue" / optional
        # required-action pages. Never press anything that cancels or links accounts.
        if '/login-actions/required-action' in urlparse(page.url or '').path:
            for selector in ('[name=cancel-aia]', '[name*=skip i]', '[id*=skip i]'):
                if await self._click(page, selector):
                    return
            await self._click(page, page.locator('a, button, input[type=submit], [role=button]').filter(has_text=self.SKIP).first)
            return
        try:
            controls = page.locator('button:visible, input[type=submit]:visible, input[type=button]:visible, a[role=button]:visible')
            count = min(await controls.count(), 25)
        except Exception:
            return
        only = None
        for index in range(count):
            control = controls.nth(index)
            try:
                label = ((await control.get_attribute('value')) or (await control.inner_text()) or '').strip()
            except Exception:
                continue
            if self.REJECT.search(label):
                continue
            if self.ACCEPT.search(label):
                await self._click(page, control)
                return
            only = control
        if only is not None and count == 1:
            await self._click(page, only)

    async def qr_step(self, context, page, job):
        data = await page.evaluate(self.script)
        if not isinstance(data, dict) or job.approval_submitted:
            return
        if data.get('error') or data.get('pageId') == 'error.ftl':
            raise ValueError('МИРЭА отклонил вход. Начните заново.')
        qr = urlparse(data.get('qr') or '')
        if not (qr.scheme == 'https' and qr.hostname == 'sso.mirea.ru' and qr.path == '/realms/mirea/qr-code-auth/scan'):
            await self._click(page, page.locator('a, button').filter(has_text=re.compile('QR', re.I)).first)
            return
        job.qr = data['qr']
        if not data.get('sessionId') or not data.get('tabId'):
            return
        response = await context.request.get('https://sso.mirea.ru/realms/mirea/qr-code-auth/check-status?' + urlencode(
            {'sessionId': data['sessionId'], 'tabId': data['tabId']}), timeout=15000, max_redirects=0)
        if response.status != 200:
            return
        try:
            result = json.loads(await response.body())
        except (ValueError, UnicodeError):
            return
        if not isinstance(result, dict):
            return
        if result.get('status') in ('timeout', 'rejected', 'session_not_found'):
            raise ValueError('QR-код истёк или вход отклонён. Начните заново.')
        if result.get('authenticated') is not True:
            return
        action = urlparse(data.get('action') or '')
        if action.scheme != 'https' or action.hostname != 'sso.mirea.ru' or action.path != '/realms/mirea/login-actions/authenticate' or not data.get('execution'):
            raise ValueError('МИРЭА изменил форму входа. Нужно обновить сервер.')
        job.qr, job.approval_submitted, job.approved_at, job.status = None, True, time.time(), 'finishing'
        await page.evaluate('''data => {
          const form = document.createElement('form');
          form.method = 'post'; form.action = data.action;
          const input = document.createElement('input');
          input.type = 'hidden'; input.name = 'authenticationExecution'; input.value = data.execution;
          form.appendChild(input); document.body.appendChild(form);
          HTMLFormElement.prototype.submit.call(form);
        }''', {'action': data['action'], 'execution': data['execution']})

    async def _login(self, job):
        context = None
        try:
            async with self.slots:
                if time.time() - job.created > self.ttl:
                    job.status = 'expired'
                    return
                context = await self._context()
                page = await context.new_page()
                # Only this worker submits the approval; the page's own polling would race it.
                await page.route('**/qr-code-auth/check-status?*', lambda route: route.abort())
                job.status = 'login'
                await page.goto(LOGIN_URL, wait_until='domcontentloaded', timeout=45000)
                while time.time() - job.created < self.ttl:
                    if job.approved_at and time.time() - job.approved_at > 90:
                        job.status, job.error = 'failed', 'Пульс не подтвердил вход. Начните заново.'
                        return
                    host = urlparse(page.url).hostname
                    if host == 'pulse.mirea.ru':
                        try:
                            identity = await asyncio.wait_for(self.identify(context), 20)
                        except Exception:
                            identity = None
                        if identity:
                            job.identity, job.status, job.qr = identity, 'complete', None
                            # Best-effort: end this MIREA session; AA ID needed it only once.
                            try:
                                await context.request.get(pulse.HOST + '/api/auth/logout?redirectUri=https%3A%2F%2Fpulse.mirea.ru', timeout=10000, max_redirects=0)
                            except Exception:
                                pass
                            return
                        await self.advance(page)
                    elif host == 'sso.mirea.ru':
                        if job.approval_submitted:
                            await self.advance(page)
                        else:
                            try:
                                await self.qr_step(context, page, job)
                            except ValueError as error:
                                job.status, job.error, job.qr = 'failed', str(error), None
                                return
                            except Exception:
                                pass  # A submitted form destroys the JS context.
                    await asyncio.sleep(2)
                job.status, job.qr = 'expired', None
        except asyncio.CancelledError:
            job.status, job.qr = 'cancelled', None
            raise
        except Exception as error:
            log.warning('login %s: %s', job.id[:6], type(error).__name__)
            job.status, job.error = 'failed', 'Не удалось войти через МИРЭА. Попробуйте ещё раз.'
        finally:
            if context:
                await context.close()

    async def close(self):
        tasks = [job.task for job in self.jobs.values() if job.task and not job.task.done()]
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        if self.browser:
            await self.browser.close()
        if self.playwright:
            await self.playwright.stop()
