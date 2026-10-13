import asyncio
import contextlib
import hmac
import json
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
from . import protocol

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
    label: str = Field(min_length=1, max_length=60)
    account_id: str | None = None
class ImportBody(BaseModel):
    cookie: str = Field(min_length=1, max_length=65536)
    label: str = Field(min_length=1, max_length=60)
    account_id: str | None = None
    expected_version: int | None = Field(default=None, ge=0)
    expected_session_version: int | None = Field(default=None, ge=0)
    group_id: int | None = None
    group_name: str | None = Field(default=None, max_length=100)
class PassBody(BaseModel):
    number: str = Field(min_length=1, max_length=19)
    expected_version: int = Field(ge=0)

class Service:
    def __init__(self, vault, transport=None):
        self.vault, self.transport = vault, transport or PulseTransport()
        self.condition = asyncio.Condition()
        self.browser = BrowserLogins(vault, self.changed)
        self.locks = defaultdict(asyncio.Lock)
        self.issued_tokens = defaultdict(dict)
        self.refresh_task = None
        self.started = time.time()

    async def changed(self):
        async with self.condition:
            self.condition.notify_all()

    async def refresh_account(self, account_id):
        async with self.locks[account_id]:
            row = self.vault.account(account_id)
            if row is None:
                return None
            secret = self.vault.secret(row)
            state, uid = await self.transport.identity(secret['cookie'])
            if state == 'valid' and uid != row['pulse_id']:
                state = 'forbidden'
            if state == 'expired' and await self.browser.refresh(row):
                return self.vault.account(account_id)
            self.vault.update_state(account_id, state, row['version'])
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
            await asyncio.sleep(max(60, int(os.environ.get('REFRESH_SECONDS', '300'))))

    async def close(self):
        if self.refresh_task:
            self.refresh_task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self.refresh_task
        await self.browser.close()
        await self.transport.close()
        self.vault.close()

def create_app(directory=None, password=None, transport=None, background=True):
    vault = Vault(directory or os.environ.get('DATA_DIR', '/data'), password or os.environ.get('ADMIN_PASSWORD', ''))
    service = Service(vault, transport)
    @asynccontextmanager
    async def lifespan(app):
        if background:
            service.refresh_task = asyncio.create_task(service.refresh_loop())
        yield
        await service.close()
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
        response = await call_next(request)
        response.headers['Cache-Control'] = 'no-store'
        response.headers['X-Content-Type-Options'] = 'nosniff'
        response.headers['Referrer-Policy'] = 'no-referrer'
        response.headers['Content-Security-Policy'] = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'"
        return response

    def panel(request, write=False):
        token = request.cookies.get('panel_session', '')
        row = vault.panel_session(token)
        if row is None:
            raise HTTPException(401, 'Sign in to the panel')
        if write and not hmac.compare_digest(request.headers.get('x-csrf-token', ''), row['csrf']):
            raise HTTPException(403, 'Invalid CSRF token')
        return row

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
        return {'ok': True, 'protocol': 2, 'sync': 'automatic'}

    @app.get('/')
    async def index():
        return FileResponse(static / 'panel.html')

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
        return {'csrf': session['csrf'], 'accounts': [vault.public(row) for row in vault.accounts()], 'cursor': vault.cursor()}

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
    async def accounts(request: Request):
        rows = vault.accounts()
        passes = [{'account_id': row['id'], 'number': vault.secret(row).get('pass_number'), 'version': row['pass_version']} for row in rows]
        return {'accounts': [vault.public(row) for row in rows], 'passes': passes, 'cursor': vault.cursor(), 'protocol': 2, 'sync': 'automatic'}

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
            return vault.public(existing)
        expected = body.expected_version if body.expected_version is not None else (None if body.expected_session_version is not None else (existing['version'] if existing else 0))
        account = vault.upsert(uid, body.label, {'cookie': cookie}, account_id=body.account_id, expected=expected, group_id=body.group_id, group_name=body.group_name, session_expected=body.expected_session_version)
        await service.changed()
        return vault.public(account)

    @app.put('/api/accounts/{identifier}/pass')
    async def publish_pass(identifier: str, body: PassBody, request: Request):
        if vault.account(identifier) is None:
            raise HTTPException(404, 'Account not found')
        try:
            account = vault.save_pass(identifier, body.number, body.expected_version)
        except ValueError:
            raise HTTPException(400, 'Invalid pass identifier') from None
        await service.changed()
        return account

    @app.get('/api/events')
    async def events(request: Request, after: int = 0, wait: int = 25):
        if after < 0 or after > 2**63 - 1 or not 0 <= wait <= 25:
            raise HTTPException(400, 'Invalid event cursor')
        deadline = time.monotonic() + wait
        while True:
            # Hold the condition while checking the database so a publisher
            # cannot signal between the check and the subscription.
            async with service.condition:
                values, cursor = vault.events(after)
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
