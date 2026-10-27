import asyncio
import contextlib
import hmac
import json
import logging
import os
import time
from collections import defaultdict, deque
from contextlib import asynccontextmanager
from pathlib import Path
from urllib.parse import urlparse
from fastapi import FastAPI, HTTPException, Request, Response
from fastapi.responses import FileResponse, JSONResponse
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, Field
from .vault import Vault, Conflict, digest
from .pulse import PulseTransport, BrowserLogins, validate_header
from .telegram import TelegramBot
from .agreements import Agreements
from . import protocol
from . import aaid as aa_id
from .pass_testing import daily_access, matches as matches_test_password

logger = logging.getLogger(__name__)

def _seconds(name, default):
    try:
        return max(60, int(os.environ.get(name, str(default))))
    except (TypeError, ValueError):
        return default

# Renew a valid session when its known expiry is within this window, re-exercise
# an active SSO state at least this often to keep it from going idle, and never
# retry a single account more often than the backoff after a failed attempt.
RENEW_BEFORE = _seconds('RENEW_BEFORE_SECONDS', 900)
RENEW_INTERVAL = _seconds('RENEW_INTERVAL_SECONDS', 900)
RENEW_BACKOFF = _seconds('RENEW_BACKOFF_SECONDS', 300)

class BodyLimitMiddleware:
    def __init__(self, app):
        self.app = app
    async def __call__(self, scope, receive, send):
        if scope['type'] != 'http':
            return await self.app(scope, receive, send)
        body = bytearray()
        while True:
            message = await receive()
            if message['type'] == 'http.disconnect':
                return
            body.extend(message.get('body', b''))
            if len(body) > 200000:
                await JSONResponse({'detail': 'Request too large'}, status_code=413)(scope, receive, send)
                return
            if not message.get('more_body', False):
                break
        replayed = False
        async def replay():
            nonlocal replayed
            if not replayed:
                replayed = True
                return {'type': 'http.request', 'body': bytes(body), 'more_body': False}
            return await receive()
        await self.app(scope, replay, send)

class LoginBody(BaseModel):
    password: str = Field(min_length=1, max_length=256)
class LoginStart(BaseModel):
    # Optional: the university sign-in flow sends no label, so it never overwrites
    # an existing account's name (and a new one falls back to a default).
    label: str | None = Field(default=None, max_length=60)
    account_id: str | None = None
class ImportBody(BaseModel):
    cookie: str = Field(min_length=1, max_length=65536)
    label: str = Field(min_length=1, max_length=60)
    account_id: str | None = None
    expected_version: int | None = Field(default=None, ge=0)
    expected_session_version: int | None = Field(default=None, ge=0)
    group_id: int | None = Field(default=None, gt=0, le=2**63 - 1)
    group_name: str | None = Field(default=None, max_length=100)
class GroupBody(BaseModel):
    group_id: int | None = Field(default=None, gt=0, le=2**63 - 1)
    group_name: str | None = Field(default=None, max_length=100)
    expected_version: int = Field(ge=0)
class PassBody(BaseModel):
    number: str = Field(min_length=1, max_length=19)
    expected_version: int = Field(ge=0)
class PassPermissionBody(BaseModel):
    allowed: bool
    expected_version: int | None = Field(default=None, ge=0)
class PagePasswordBody(BaseModel):
    password: str = Field(min_length=4, max_length=128)
class LabelBody(BaseModel):
    label: str = Field(min_length=1, max_length=60)
    expected_version: int | None = Field(default=None, ge=0)

class Service:
    def __init__(self, vault, transport=None):
        self.vault, self.transport = vault, transport or PulseTransport()
        self.condition = asyncio.Condition()
        self.browser = BrowserLogins(vault, self.changed)
        self.locks = defaultdict(asyncio.Lock)
        self.issued_tokens = defaultdict(dict)
        self.agreements = Agreements(vault)
        self.telegram = None
        self.refresh_task = None
        self.started = time.time()
        self.renewal_attempts = {}

    async def changed(self):
        async with self.condition:
            self.condition.notify_all()

    async def refresh_account(self, account_id):
        async with self.locks[account_id]:
            row = self.vault.account(account_id)
            if row is None:
                return None
            secret = self.vault.secret(row)
            if not secret.get('cookie'):
                # Session was ended/cleared: nothing to check or renew, keep it expired.
                self.vault.update_state(account_id, 'expired', row['version'])
                await self.changed()
                return self.vault.account(account_id)
            state, uid = await self.transport.identity(secret['cookie'])
            if state == 'valid' and uid != row['pulse_id']:
                state = 'forbidden'
            now = time.time()
            # Browser SSO must be exercised well before its idle expiry, not only
            # after the Pulse cookie has already stopped working. Keeping it warm
            # and rolling the saved state forward on every pass is what lets a
            # session stay alive for days without any human re-login.
            expiry = row['expires_at']
            due = state == 'expired' or (state == 'valid' and (
                (expiry is not None and expiry <= now + RENEW_BEFORE) or now - secret.get('renewed_at', 0) >= RENEW_INTERVAL))
            last_attempt = self.renewal_attempts.get(account_id, 0)
            if due and secret.get('storage_state') and now - last_attempt >= RENEW_BACKOFF:
                self.renewal_attempts[account_id] = now
                if await self.browser.refresh(row):
                    row = self.vault.account(account_id) or row
                    secret = self.vault.secret(row)
            self.vault.update_state(account_id, state, row['version'])
            # Auto-detect the group from the account itself the first time, without
            # clobbering a group someone set by hand.
            fresh = self.vault.account(account_id)
            detect = getattr(self.transport, 'subscribed_group', None)
            if state == 'valid' and fresh is not None and fresh['group_id'] is None and detect is not None:
                try:
                    group_id, group_name = await detect(secret['cookie'])
                    if group_id:
                        self.vault.update_group(account_id, group_id, group_name, fresh['version'])
                except Exception:
                    pass
            await self.changed()
            return self.vault.account(account_id)

    async def refresh_loop(self):
        while True:
            for row in self.vault.accounts():
                try:
                    await self.refresh_account(row['id'])
                except asyncio.CancelledError:
                    raise
                except Exception:
                    # Leave last-known credentials intact after transient failures.
                    pass
            await asyncio.sleep(max(60, int(os.environ.get('REFRESH_SECONDS', '180'))))

    async def close(self):
        if self.refresh_task:
            self.refresh_task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self.refresh_task
        await self.browser.close()
        await self.transport.close()
        self.vault.close()

def create_app(directory=None, password=None, transport=None, background=True, aaid=None):
    # AA ID sign-in is enforced on the app API when a verifier is configured
    # (AAID_JWKS_URL), and left off for development servers without AA ID.
    aaid = aaid if aaid is not None else aa_id.from_environment(os.environ)
    vault = Vault(directory or os.environ.get('DATA_DIR', '/data'), password or os.environ.get('ADMIN_PASSWORD', ''))
    service = Service(vault, transport)
    bot = None
    @asynccontextmanager
    async def lifespan(app):
        nonlocal bot
        if background:
            service.refresh_task = asyncio.create_task(service.refresh_loop())
            token = os.environ.get('TELEGRAM_TOKEN', '').strip()
            if token:
                try:
                    bot = TelegramBot(token, service, os.environ.get('PUBLIC_URL', ''),
                                      relay_url=os.environ.get('TELEGRAM_RELAY_URL', '').strip(),
                                      relay_key=os.environ.get('TELEGRAM_RELAY_KEY', '').strip(),
                                      proxy_url=os.environ.get('TELEGRAM_PROXY_URL', '').strip(),
                                      proxy_key=os.environ.get('TELEGRAM_PROXY_KEY', '').strip())
                except ValueError as error:
                    logger.error('Telegram bot disabled by invalid configuration: %s', error)
                else:
                    app.state.telegram = bot
                    service.telegram = bot
                    bot.task = asyncio.create_task(bot.run())
        yield
        if bot is not None:
            bot.running = False
            bot.task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await bot.task
            await bot.close()
        await service.close()
        if aaid is not None:
            await aaid.close()
    app = FastAPI(title='AntiAttendance session server', docs_url=None, redoc_url=None, openapi_url=None, lifespan=lifespan)
    app.add_middleware(BodyLimitMiddleware)
    app.state.service = service
    static = Path(__file__).resolve().parents[1] / 'static'
    app.mount('/static', StaticFiles(directory=static), name='static')
    attempts = defaultdict(deque)

    @app.middleware('http')
    async def security(request, call_next):
        size = request.headers.get('content-length')
        if size and (not size.isdigit() or int(size) > 200000):
            return JSONResponse({'detail': 'Request too large'}, status_code=413)
        if request.url.path.startswith('/api/') and request.method not in ('GET', 'HEAD'):
            origin = request.headers.get('origin')
            if origin and urlparse(origin).netloc != request.headers.get('host'):
                return JSONResponse({'detail': 'Invalid origin'}, status_code=403)
        request.state.aaid = None
        if aaid is not None and request.url.path.startswith('/api/') and not browser_allowed(request):
            scheme, _, token = request.headers.get('authorization', '').partition(' ')
            claims = await aaid.verify(token) if scheme.lower() == 'bearer' and token else None
            if claims is None and (aaid.enforce or request.url.path.startswith('/api/me/')):
                return JSONResponse({'detail': 'AA ID sign-in required', 'aaid_url': aaid.public_url},
                                    status_code=401, headers={'WWW-Authenticate': 'Bearer', 'X-AAID-Required': '1', 'Cache-Control': 'no-store'})
            request.state.aaid = claims
        response = await call_next(request)
        response.headers['Cache-Control'] = 'no-store'
        response.headers['X-Content-Type-Options'] = 'nosniff'
        response.headers['Referrer-Policy'] = 'no-referrer'
        response.headers['Content-Security-Policy'] = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'"
        return response

    def browser_allowed(request):
        path = request.url.path
        if any(pattern.match(path) for pattern in aa_id.BROWSER_OPEN):
            return True
        owner = aa_id.BROWSER_OWNER.match(path)
        return owner is not None and not request.headers.get('authorization') and page_authed(request, owner.group(1))

    def panel(request, write=False):
        token = request.cookies.get('panel_session', '')
        row = vault.panel_session(token)
        if row is None:
            raise HTTPException(401, 'Sign in to the panel')
        if write and not hmac.compare_digest(request.headers.get('x-csrf-token', ''), row['csrf']):
            raise HTTPException(403, 'Invalid CSRF token')
        return row

    def page_authed(request, account_id):
        return vault.page_session_account(request.cookies.get('page_session', '')) == account_id

    def test_access(request):
        password = request.headers.get('x-pass-test-key', '')
        if not password:
            return None
        access = daily_access(vault.pass_testing_key)
        return access if matches_test_password(password, access) else None

    def pass_access(request, identifier):
        return page_authed(request, identifier) or service.agreements.status(identifier, request.headers.get('x-pass-client', '')) == 'approved'

    def require_pass(request, row, operation):
        if row['pass_allowed'] and pass_access(request, row['id']):
            return
        if test_access(request) is not None:
            vault.audit('pass_testing_' + operation, 'daily-admin', row['id'])
            return
        raise HTTPException(403, 'Owner approval in Telegram is required for this person',
                            headers={'X-Pass-Test-Invalid': '1'} if request.headers.get('x-pass-test-key') else None)

    def set_page_cookie(response, token):
        secure = os.environ.get('PUBLIC_URL', '').startswith('https://')
        response.set_cookie('page_session', token, max_age=2592000, httponly=True, secure=secure, samesite='lax', path='/')
        return response

    async def bounded_body(request, limit=65536):
        result = bytearray()
        async for part in request.stream():
            if len(result) + len(part) > limit:
                raise HTTPException(413, 'Request too large')
            result.extend(part)
        return bytes(result)

    @app.exception_handler(Conflict)
    async def conflict(request, exception):
        return JSONResponse({'detail': str(exception)}, status_code=409)

    @app.get('/health')
    async def health():
        return {'ok': True, 'protocol': 2, 'sync': 'automatic',
                'aaid': {'required': aaid.enforce, 'url': aaid.public_url} if aaid is not None else {'required': False}}

    @app.get('/')
    async def index():
        return FileResponse(static / 'picker.html')

    @app.get('/panel')
    async def panel_page():
        return FileResponse(static / 'panel.html')

    @app.get('/login')
    async def login_page():
        return FileResponse(static / 'login.html')

    @app.get('/u/{identifier}')
    async def user_page(identifier: str):
        # Each account's own page. The identifier is the account's opaque UUID,
        # which acts as the capability for this page under the URL-only model.
        return FileResponse(static / 'user.html')

    @app.post('/panel/api/login')
    async def login(body: LoginBody, request: Request):
        origin = request.headers.get('origin')
        if origin and urlparse(origin).netloc != request.headers.get('host'):
            raise HTTPException(403, 'Invalid origin')
        address = request.client.host if request.client else 'unknown'
        queue = attempts[address]
        now = time.time()
        while queue and queue[0] < now - 60:
            queue.popleft()
        if len(queue) >= 5:
            raise HTTPException(429, 'Try again in a minute')
        queue.append(now)
        result = vault.admin_login(body.password)
        if not result:
            raise HTTPException(401, 'Wrong panel password')
        token, csrf = result
        response = JSONResponse({'csrf': csrf})
        secure = os.environ.get('PUBLIC_URL', '').startswith('https://')
        response.set_cookie('panel_session', token, max_age=43200, httponly=True, secure=secure, samesite='strict', path='/panel')
        return response

    @app.get('/panel/api/state')
    async def panel_state(request: Request):
        session = panel(request)
        accounts = []
        for row in vault.accounts():
            metadata = vault.public(row)
            secret = vault.secret(row)
            metadata['automatic_renewal'] = bool(secret.get('storage_state'))
            metadata['renewed_at'] = secret.get('renewed_at')
            metadata['page_seen_at'] = row['page_seen_at'] if 'page_seen_at' in row.keys() else None
            accounts.append(metadata)
        return {'csrf': session['csrf'], 'accounts': accounts, 'cursor': vault.cursor(),
                'pass_testing': daily_access(vault.pass_testing_key)}

    @app.post('/api/debug/pass-testing')
    async def unlock_pass_testing(body: LoginBody, request: Request):
        address = request.client.host if request.client else 'unknown'
        queue = attempts[('pass-testing', address)]
        now = time.time()
        while queue and queue[0] < now - 300:
            queue.popleft()
        if len(queue) >= 8:
            raise HTTPException(429, 'Too many attempts; wait a few minutes')
        queue.append(now)
        access = daily_access(vault.pass_testing_key)
        if not matches_test_password(body.password, access):
            raise HTTPException(401, 'Wrong or expired pass testing password')
        vault.audit('pass_testing_unlocked', 'daily-admin:' + address)
        return {'expires_at': access['expires_at'], 'day': access['day']}

    @app.post('/panel/api/logout')
    async def logout(request: Request):
        panel(request, True)
        vault.logout(request.cookies.get('panel_session', ''))
        response = JSONResponse({'ok': True})
        response.delete_cookie('panel_session', path='/panel')
        return response

    @app.post('/panel/api/logins')
    async def start_login(body: LoginStart, request: Request):
        panel(request, True)
        if body.account_id and vault.account(body.account_id) is None:
            raise HTTPException(404, 'Account not found')
        try:
            job, token = service.browser.start(body.label, body.account_id)
        except ValueError as error:
            raise HTTPException(429, str(error)) from None
        return {'id': job.id, 'enrollment_path': '/enroll/' + job.id + '/' + token, 'expires_in': 600}

    @app.post('/api/logins')
    async def public_start_login(body: LoginStart, request: Request):
        # Self-service: an account owner opens the picker, chooses their account
        # (or adds a new one) and starts a login without the panel password. This
        # matches the URL-is-the-capability model; destructive actions still need
        # the panel. The enrollment job is reached through the public /enroll token.
        # Soft anti-spam only. Behind the bundled Nginx every caller shares one
        # source address, so this is effectively a global burst limit; the real
        # resource guard is the browser's active-login cap below.
        address = request.client.host if request.client else 'unknown'
        queue = attempts[('public-login', address)]
        now = time.time()
        while queue and queue[0] < now - 180:
            queue.popleft()
        if len(queue) >= 40:
            raise HTTPException(429, 'Too many login attempts right now; wait a minute')
        queue.append(now)
        if body.account_id and vault.account(body.account_id) is None:
            raise HTTPException(404, 'Account not found')
        try:
            job, token = service.browser.start(body.label, body.account_id)
        except ValueError as error:
            raise HTTPException(429, str(error)) from None
        return {'id': job.id, 'enrollment_path': '/enroll/' + job.id + '/' + token, 'expires_in': 600}

    def login_job(identifier):
        job = service.browser.jobs.get(identifier)
        if job is None or time.time() - job.created > 900:
            raise HTTPException(404, 'Login has expired')
        return job

    def public_job(identifier, token):
        job = login_job(identifier)
        if time.time() - job.created > 600 or not hmac.compare_digest(digest(token), job.capability_hash):
            raise HTTPException(404, 'Login not found')
        return job

    def job_state(job):
        return {'status': job.status, 'qr': job.qr, 'error': job.error, 'account_id': job.result_id, 'label': job.label}

    @app.get('/panel/api/logins/{identifier}')
    async def login_state(identifier: str, request: Request):
        panel(request)
        return job_state(login_job(identifier))

    @app.get('/panel/api/logins/{identifier}/qr')
    async def login_qr(identifier: str, request: Request):
        panel(request)
        png = service.browser.qr_png(login_job(identifier))
        if png is None:
            raise HTTPException(404, 'QR is not ready')
        return Response(png, media_type='image/png')

    @app.delete('/panel/api/logins/{identifier}')
    async def cancel_login(identifier: str, request: Request):
        panel(request, True)
        job = login_job(identifier)
        if job.task and not job.task.done():
            job.task.cancel()
        return {'ok': True}

    @app.get('/enroll/{identifier}/{token}')
    async def owner_login(identifier: str, token: str):
        public_job(identifier, token)
        return FileResponse(static / 'enroll.html')

    @app.get('/enroll/{identifier}/{token}/state')
    async def owner_state(identifier: str, token: str):
        return job_state(public_job(identifier, token))

    @app.get('/enroll/{identifier}/{token}/qr')
    async def owner_qr(identifier: str, token: str):
        png = service.browser.qr_png(public_job(identifier, token))
        if png is None:
            raise HTTPException(404, 'QR is not ready')
        return Response(png, media_type='image/png')

    @app.post('/enroll/{identifier}/{token}/claim')
    async def owner_claim(identifier: str, token: str):
        # After "sign in via university" completes, the holder of this enrollment
        # token (who just approved the login in MIREA) gets a page session for the
        # account it produced, and is sent to that account's page.
        job = public_job(identifier, token)
        if job.status != 'complete' or not job.result_id:
            raise HTTPException(409, 'Login is not complete yet')
        return set_page_cookie(JSONResponse({'account_id': job.result_id}), vault.create_page_session(job.result_id))

    @app.post('/panel/api/accounts/{identifier}/refresh')
    async def refresh_account(identifier: str, request: Request):
        panel(request, True)
        account = await service.refresh_account(identifier)
        if account is None:
            raise HTTPException(404, 'Account not found')
        return vault.public(account)

    @app.delete('/panel/api/accounts/{identifier}')
    async def delete_account(identifier: str, request: Request):
        panel(request, True)
        vault.delete_account(identifier)
        await service.changed()
        return {'ok': True}

    @app.get('/api/accounts')
    async def accounts(request: Request, group_id: int | None = None):
        if group_id is not None and not 0 < group_id <= 2**63 - 1:
            raise HTTPException(400, 'Invalid group')
        rows = vault.accounts(group_id)
        testing = test_access(request)
        key = request.headers.get('x-pass-client', '')
        caller = request.state.aaid
        # `own` marks the account of the signed-in AA ID person (same MIREA identity).
        metadata = [{**vault.public(row), 'pass_access': service.agreements.status(row['id'], key),
                     'own': caller is not None and row['pulse_id'] == caller['mirea']} for row in rows]
        passes = [{'account_id': row['id'], 'number': vault.secret(row).get('pass_number') if testing or (row['pass_allowed'] and pass_access(request, row['id'])) else None, 'version': row['pass_version']} for row in rows]
        return {'accounts': metadata, 'passes': passes, 'cursor': vault.cursor(), 'protocol': 2, 'sync': 'automatic', 'group_filter': group_id,
                'pass_agreements_required': True, 'pass_access_expires_at': time.time() + 300,
                'pass_testing_expires_at': testing['expires_at'] if testing else None}

    @app.post('/api/accounts/{identifier}/pass-request')
    async def request_pass(identifier: str, request: Request):
        bot = service.telegram
        if bot is None or not bot.username:
            raise HTTPException(503, 'Telegram bot is not configured or ready')
        address = request.client.host if request.client else 'unknown'
        queue = attempts[('pass-request', address)]
        now = time.time()
        while queue and queue[0] < now - 300:
            queue.popleft()
        if len(queue) >= 40:
            raise HTTPException(429, 'Too many requests; try again later')
        queue.append(now)
        try:
            identifier = service.agreements.create(identifier, request.headers.get('x-pass-client', ''))
        except KeyError:
            raise HTTPException(404, 'Account not found') from None
        except ValueError as error:
            raise HTTPException(400, str(error)) from None
        return {'url': 'https://t.me/' + bot.username + '?start=access_' + identifier, 'expires_in': 600}

    @app.get('/api/groups')
    async def groups():
        return {'groups': vault.groups()}

    @app.get('/api/groups/search')
    async def groups_search(q: str = ''):
        # Live group lookup for the picker/user page: name -> numeric schedule id
        # via the public MIREA schedule API. Open, like the rest of /api.
        query = q.strip()
        if len(query) < 2:
            return {'groups': []}
        return {'groups': await service.transport.search_groups(query[:50])}

    @app.get('/panel/api/accounts/{identifier}/profile-fields')
    async def profile_fields(identifier: str, request: Request):
        # Admin-only diagnostic: the decoded Pulse profile protobuf as a field tree,
        # for discovering where the group lives. Cookies/tokens are never included —
        # only the profile message is decoded, not the stored secret.
        panel(request)
        row = vault.account(identifier)
        if row is None:
            raise HTTPException(404, 'Account not found')
        secret = vault.secret(row)
        if not secret.get('cookie'):
            raise HTTPException(409, 'The session was ended; sign in again')
        try:
            reply = await service.transport.rpc(secret['cookie'], 'profile', protocol.profile_request())
        except Exception:
            raise HTTPException(503, 'Pulse is unavailable') from None
        try:
            message, status = protocol.grpc_message(reply.body, reply.headers)
        except ValueError:
            raise HTTPException(502, 'Could not parse the Pulse profile response') from None
        return {'grpc_status': status, 'fields': protocol.decode_tree(message) if message is not None else {}}

    @app.get('/api/accounts/{identifier}')
    async def account_detail(identifier: str, request: Request):
        row = vault.account(identifier)
        if row is None:
            raise HTTPException(404, 'Account not found')
        if not page_authed(request, identifier):
            raise HTTPException(401, 'Sign in to this account first')
        return vault.public(row)

    @app.get('/api/me/attendance')
    async def my_attendance(request: Request, days: int = 7):
        # Turnstile entries and exits of the AA ID caller's own account (same MIREA
        # person), newest first. Used by the AA ID site; never another person's data.
        caller = request.state.aaid
        if caller is None:
            raise HTTPException(401, 'AA ID sign-in required')
        if not 1 <= days <= 14:
            raise HTTPException(400, 'Invalid period')
        row = next((item for item in vault.accounts() if item['pulse_id'] == caller['mirea']), None)
        if row is None:
            raise HTTPException(404, 'This MIREA account is not connected to the session server')
        cookie = vault.secret(row).get('cookie')
        if not cookie:
            raise HTTPException(409, 'The session was ended; sign in again')
        now = time.time()
        today = (int((now + 3 * 3600) // 86400)) * 86400 - 3 * 3600  # Moscow midnight
        result = await service.transport.acs_timeline(cookie, row['pulse_id'], today - (days - 1) * 86400, today + 86400 - 1)
        if result is None:
            raise HTTPException(503, 'Pulse is unavailable; try again')
        events, inside = result
        events = sorted(events, key=lambda e: e['time'], reverse=True)
        last = lambda direction: next((e for e in events if e['direction'] == direction), None)
        return {'account_id': row['id'], 'state': row['state'], 'inside': inside,
                'last_in': last('in'), 'last_out': last('out'), 'events': events[:30]}

    @app.get('/api/accounts/{identifier}/attendance')
    @app.get('/panel/api/accounts/{identifier}/attendance')
    async def account_attendance(identifier: str, request: Request):
        # Turnstile entry/exit for today (Moscow time). Owner's page session, or panel.
        if request.url.path.startswith('/panel/'):
            panel(request)
        elif not page_authed(request, identifier):
            raise HTTPException(401, 'Sign in to this account first')
        row = vault.account(identifier)
        if row is None:
            raise HTTPException(404, 'Account not found')
        now = time.time()
        start = (int((now + 3 * 3600) // 86400)) * 86400 - 3 * 3600  # Moscow midnight today
        end = start + 86400 - 1
        cookie = vault.secret(row).get('cookie')
        if not cookie:
            raise HTTPException(409, 'The session was ended; sign in again')
        result = await service.transport.acs_timeline(cookie, row['pulse_id'], start, end)
        if result is None:
            raise HTTPException(503, 'Pulse is unavailable; try again')
        events, inside = result
        return {'inside': inside, 'events': events, 'since': start}

    @app.get('/api/accounts/{identifier}/page')
    async def account_page_state(identifier: str, request: Request):
        row = vault.account(identifier)
        if row is None:
            raise HTTPException(404, 'Account not found')
        if page_authed(request, identifier):
            data = vault.public(row)
            data['authed'] = True
            data['has_password'] = vault.has_page_password(identifier)
            return data
        return {'authed': False, 'has_password': vault.has_page_password(identifier),
                'id': identifier, 'label': row['label']}

    @app.post('/api/accounts/{identifier}/page/password')
    async def account_set_page_password(identifier: str, body: PagePasswordBody, request: Request):
        if vault.account(identifier) is None:
            raise HTTPException(404, 'Account not found')
        # Setting OR changing the password always requires an active session, so a
        # passwordless account cannot be hijacked by whoever sets a password first.
        # The only way to get that first session is signing in via the university
        # (the /claim path), which proves the account is yours.
        if not page_authed(request, identifier):
            raise HTTPException(401, 'Sign in via the university first to set a password')
        vault.set_page_password(identifier, body.password)
        return JSONResponse({'ok': True, 'authed': True})

    @app.post('/api/accounts/{identifier}/page/login')
    async def account_page_login(identifier: str, body: PagePasswordBody, request: Request):
        if vault.account(identifier) is None:
            raise HTTPException(404, 'Account not found')
        address = request.client.host if request.client else 'unknown'
        queue = attempts[('page-login', identifier, address)]
        now = time.time()
        while queue and queue[0] < now - 300:
            queue.popleft()
        if len(queue) >= 10:
            raise HTTPException(429, 'Too many attempts; wait a few minutes')
        queue.append(now)
        if not vault.verify_page_password(identifier, body.password):
            raise HTTPException(401, 'Wrong password')
        return set_page_cookie(JSONResponse({'ok': True, 'authed': True}), vault.create_page_session(identifier))

    @app.post('/api/accounts/{identifier}/page/logout')
    async def account_page_logout(identifier: str, request: Request):
        vault.delete_page_session(request.cookies.get('page_session', ''))
        response = JSONResponse({'ok': True})
        response.delete_cookie('page_session', path='/')
        return response

    @app.put('/api/accounts/{identifier}/label')
    @app.put('/panel/api/accounts/{identifier}/label')
    async def rename_account(identifier: str, body: LabelBody, request: Request):
        # Renaming is allowed only from the admin panel or from the owner's own
        # page (a valid page session for this account).
        if request.url.path.startswith('/panel/'):
            panel(request, True)
        elif not page_authed(request, identifier):
            raise HTTPException(401, 'Sign in to this account first')
        try:
            account = vault.rename(identifier, body.label, body.expected_version)
        except KeyError:
            raise HTTPException(404, 'Account not found') from None
        except ValueError:
            raise HTTPException(400, 'Invalid name') from None
        await service.changed()
        return account

    @app.post('/api/accounts/{identifier}/end-session')
    @app.post('/panel/api/accounts/{identifier}/end-session')
    async def end_session(identifier: str, request: Request):
        # Terminate the Pulse session (keep the account). Owner page session or panel.
        if request.url.path.startswith('/panel/'):
            panel(request, True)
        elif not page_authed(request, identifier):
            raise HTTPException(401, 'Sign in to this account first')
        async with service.locks[identifier]:
            row = vault.account(identifier)
            if row is None:
                raise HTTPException(404, 'Account not found')
            cookie = vault.secret(row).get('cookie')
            if cookie:
                try:
                    await service.transport.logout(cookie)
                except Exception:
                    # Local revocation must succeed even when upstream logout fails.
                    pass
            account = vault.end_session(identifier)
            service.issued_tokens.pop(identifier, None)
            service.renewal_attempts.pop(identifier, None)
            vault.audit('session_ended', 'panel' if request.url.path.startswith('/panel/') else 'owner', identifier)
            await service.changed()
            return account

    @app.put('/api/accounts/{identifier}/pass-allowed')
    @app.put('/panel/api/accounts/{identifier}/pass-allowed')
    async def set_pass_permission(identifier: str, body: PassPermissionBody, request: Request):
        if request.url.path.startswith('/panel/'):
            panel(request, True)
        elif not page_authed(request, identifier):
            raise HTTPException(401, 'Sign in to this account first')
        try:
            account = vault.set_pass_allowed(identifier, body.allowed, body.expected_version)
        except KeyError:
            raise HTTPException(404, 'Account not found') from None
        await service.changed()
        return account

    @app.put('/api/accounts/{identifier}/group')
    @app.put('/panel/api/accounts/{identifier}/group')
    async def assign_group(identifier: str, body: GroupBody, request: Request):
        if request.url.path.startswith('/panel/'):
            panel(request, True)
        try:
            account = vault.update_group(identifier, body.group_id, body.group_name, body.expected_version)
        except KeyError:
            raise HTTPException(404, 'Account not found') from None
        await service.changed()
        return account

    @app.post('/api/accounts/import')
    async def import_account(body: ImportBody, request: Request):
        try:
            cookie = validate_header(body.cookie)
        except ValueError:
            raise HTTPException(400, 'Invalid cookie') from None
        state, uid = await service.transport.identity(cookie)
        if state != 'valid':
            raise HTTPException(422, 'Pulse did not confirm this session')
        existing = next((row for row in vault.accounts() if row['pulse_id'] == uid), None)
        if body.account_id and (existing is None or existing['id'] != body.account_id):
            raise Conflict('This session belongs to a different Pulse account')
        if existing and body.expected_version is None and body.expected_session_version is None and existing['state'] == 'valid':
            # An unlinked phone must adopt the authoritative server session,
            # not overwrite it with an indistinguishable opaque old cookie.
            if existing['group_id'] is None and body.group_id is not None:
                metadata = vault.update_group(existing['id'], body.group_id, body.group_name, existing['version'])
                await service.changed()
                return metadata
            return vault.public(existing)
        expected = body.expected_version if body.expected_version is not None else (None if body.expected_session_version is not None else (existing['version'] if existing else 0))
        account = vault.upsert(uid, body.label, {'cookie': cookie}, account_id=body.account_id, expected=expected, group_id=body.group_id if existing is None or existing['group_id'] is None else None, group_name=body.group_name if existing is None or existing['group_id'] is None else None, session_expected=body.expected_session_version)
        await service.changed()
        return vault.public(account)

    @app.put('/api/accounts/{identifier}/pass')
    async def publish_pass(identifier: str, body: PassBody, request: Request):
        row = vault.account(identifier)
        if row is None:
            raise HTTPException(404, 'Account not found')
        require_pass(request, row, 'publish')
        try:
            account = vault.save_pass(identifier, body.number, body.expected_version)
        except ValueError:
            raise HTTPException(400, 'Invalid pass identifier') from None
        await service.changed()
        return account

    @app.get('/api/events')
    async def events(request: Request, after: int = 0, wait: int = 25, group_id: int | None = None):
        if after < 0 or after > 2**63 - 1 or not 0 <= wait <= 25:
            raise HTTPException(400, 'Invalid event cursor')
        if group_id is not None and not 0 < group_id <= 2**63 - 1:
            raise HTTPException(400, 'Invalid group')
        deadline = time.monotonic() + wait
        while True:
            # Hold the condition while checking the database so a publisher
            # cannot signal between the check and the subscription.
            async with service.condition:
                values, cursor = vault.events(after, group_id)
                if values or cursor != after or time.monotonic() >= deadline:
                    return {'events': values, 'cursor': cursor}
                try:
                    await asyncio.wait_for(service.condition.wait(), max(.01, deadline - time.monotonic()))
                except asyncio.TimeoutError:
                    return {'events': [], 'cursor': cursor}

    @app.post('/api/accounts/{identifier}/rpc/{method}')
    async def rpc(identifier: str, method: str, request: Request):
        if method not in protocol.METHODS:
            raise HTTPException(404, 'RPC not allowed')
        row = vault.account(identifier)
        if row is None:
            raise HTTPException(404, 'Account not found')
        if method in ('pass-token', 'pass-code', 'pass-issue'):
            require_pass(request, row, method)
        if method in ('pass-code', 'pass-issue'):
            limit_key = (method, identifier)
            queue = attempts[limit_key]
            now = time.time()
            while queue and queue[0] < now - 600:
                queue.popleft()
            if len(queue) >= (6 if method == 'pass-code' else 20):
                raise HTTPException(429, 'Too many verification attempts; wait before retrying')
            queue.append(now)
        body = await bounded_body(request)
        if len(body) < 5 or body[0] != 0 or int.from_bytes(body[1:5], 'big') != len(body) - 5:
            raise HTTPException(400, 'Invalid unary gRPC request')
        secret = vault.secret(row)
        if not secret.get('cookie'):
            raise HTTPException(409, 'The session was ended; sign in again')
        authorization = None
        if method in ('pass-code', 'pass-issue'):
            authorization = request.headers.get('x-pulse-token', '')
            expiry = service.issued_tokens[identifier].get(digest(authorization), 0)
            if not authorization or expiry <= time.time():
                raise HTTPException(403, 'Obtain a fresh token for this account first')
        try:
            reply = await service.transport.rpc(secret['cookie'], method, body, authorization)
        except Exception:
            raise HTTPException(503, 'Pulse is unavailable; the stored session was retained') from None
        if method == 'profile':
            state, uid = protocol.authentication_state(reply.status, reply.headers, reply.body)
            if state == 'valid' and uid != row['pulse_id']:
                raise HTTPException(503, 'Account identity mismatch')
            vault.update_state(identifier, state, row['version'])
            await service.changed()
        if reply.status == 200 and method in ('pass-token', 'pass-issue'):
            try:
                message, grpc_status = protocol.grpc_message(reply.body, reply.headers)
                if grpc_status == 0 and message is not None:
                    values = protocol.fields(message)
                    if method == 'pass-token':
                        token = values[1].decode()
                        expiry = protocol.token_expiry(token)
                        tokens = service.issued_tokens[identifier]
                        for value, expires in list(tokens.items()):
                            if expires <= time.time():
                                del tokens[value]
                        if len(tokens) > 100:
                            tokens.clear()
                        tokens[digest(token)] = expiry
                    elif 1 in values:
                        number = protocol.fields(values[1]).get(1)
                        if type(number) is int and number > 0:
                            vault.save_pass(identifier, str(number), expected=row['pass_version'])
                            await service.changed()
            except (Conflict, ValueError, KeyError, TypeError, UnicodeError):
                pass
        headers = {key: reply.headers[key] for key in ('content-type', 'grpc-status', 'grpc-message', 'location') if key in reply.headers}
        # Device authentication errors above use HTTP 401. Pulse's auth rejection
        # remains a gRPC UNAUTHENTICATED result to distinguish the two layers.
        if reply.status == 401:
            return Response(protocol.frame(b'grpc-status: 16\r\n', 128), media_type='application/grpc-web+proto')
        return Response(reply.body, status_code=reply.status, headers=headers)

    return app
