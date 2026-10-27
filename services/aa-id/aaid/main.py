import os
import re
import time
from collections import defaultdict, deque
from contextlib import asynccontextmanager
import base64
import secrets
from datetime import datetime, timedelta, timezone
import hmac
from pathlib import Path
from urllib.parse import urlencode, urlparse
import httpx
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import FileResponse, JSONResponse
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, Field
from .pulse import PulseIdentity
from . import schedule
from .store import Store, Blocked, digest
from .tokens import Signer, verify

def _number(name, default, minimum=1):
    try:
        return max(minimum, int(os.environ.get(name, str(default))))
    except (TypeError, ValueError):
        return default

class MireaSignIn(BaseModel):
    # The Pulse session cookie from a MIREA sign-in on the device. It is used once
    # to ask Pulse who the person is and is never stored or logged.
    pulse_cookie: str = Field(min_length=1, max_length=65536)
    device: str = Field(default='', max_length=80)

class RefreshBody(BaseModel):
    refresh_token: str = Field(min_length=20, max_length=200)

class ClaimBody(BaseModel):
    token: str = Field(min_length=20, max_length=200)

class AuthorizeBody(BaseModel):
    state: str = Field(pattern=r'^[A-Za-z0-9_-]{16,128}$')

class CodeBody(BaseModel):
    code: str = Field(min_length=20, max_length=200)

class StatusBody(BaseModel):
    status: str = Field(pattern='^(active|blocked)$')

def browser_name(agent):
    """A short label such as 'Chrome · Android' for the device list."""
    browser = next((name for token, name in (('YaBrowser', 'Яндекс Браузер'), ('Edg/', 'Edge'), ('OPR/', 'Opera'),
        ('Firefox/', 'Firefox'), ('Chrome/', 'Chrome'), ('Safari/', 'Safari')) if token in agent), 'Браузер')
    system = next((name for token, name in (('Android', 'Android'), ('iPhone', 'iPhone'), ('iPad', 'iPad'),
        ('Windows', 'Windows'), ('Mac OS X', 'macOS'), ('Linux', 'Linux')) if token in agent), None)
    return browser + (' · ' + system if system else '')

def create_app(directory=None, identity=None, logins=None, http=None):
    directory = directory or os.environ.get('DATA_DIR', '/data')
    issuer = os.environ.get('AAID_ISSUER', 'aa-id')
    audience = os.environ.get('AAID_AUDIENCE', 'aa')
    access_ttl = _number('ACCESS_TTL_SECONDS', 600, 60)
    session_ttl = _number('SESSION_TTL_SECONDS', 60 * 86400, 3600)
    max_sessions = _number('MAX_SESSIONS', 2)
    trust_proxy = os.environ.get('TRUST_PROXY_IP', '') == '1'
    admin_token = os.environ.get('ADMIN_TOKEN', '')
    store, signer = Store(directory), Signer(directory)
    pulse = identity or PulseIdentity()
    if logins is None:
        from .mirea_login import MireaLogins
        logins = MireaLogins(slots=_number('LOGIN_SLOTS', 4), max_active=_number('MAX_ACTIVE_LOGINS', 16),
                             ttl=_number('LOGIN_TTL_SECONDS', 600, 120))
    secure_cookie = os.environ.get('AAID_PUBLIC_URL', '').startswith('https://')
    web_ttl = _number('WEB_SESSION_TTL_SECONDS', 7 * 86400, 600)
    attempts = defaultdict(deque)
    static = Path(__file__).with_name('static')
    # The session server, reached inside the Compose network. It holds the Pulse
    # sessions; AA ID asks it only for the signed-in person's own turnstile log.
    sessions_url = os.environ.get('SESSIONS_URL', 'http://sessions:8080').rstrip('/')
    http = http or httpx.AsyncClient(timeout=20, follow_redirects=False)
    calendars = {}
    # The AntiAttendance site that signs people in with AA ID, and its one-time codes.
    site_url = os.environ.get('AA_SITE_URL', '').strip().rstrip('/')
    codes = {}

    @asynccontextmanager
    async def lifespan(app):
        yield
        await logins.close()
        await http.aclose()
        await pulse.close()
        store.close()

    app = FastAPI(title='AA ID', docs_url=None, redoc_url=None, openapi_url=None, lifespan=lifespan)
    app.state.store, app.state.signer, app.state.logins = store, signer, logins
    app.mount('/static', StaticFiles(directory=static), name='static')

    @app.middleware('http')
    async def headers(request, call_next):
        size = request.headers.get('content-length')
        if size and (not size.isdigit() or int(size) > 100000):
            return JSONResponse({'detail': 'Request too large'}, status_code=413)
        if request.url.path.startswith('/web/') and request.method not in ('GET', 'HEAD'):
            origin = request.headers.get('origin')
            if origin and urlparse(origin).netloc != request.headers.get('host'):
                return JSONResponse({'detail': 'Invalid origin'}, status_code=403)
        response = await call_next(request)
        response.headers['Cache-Control'] = 'no-store'
        response.headers['Content-Security-Policy'] = "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
        response.headers['X-Frame-Options'] = 'DENY'
        response.headers['X-Content-Type-Options'] = 'nosniff'
        response.headers['Referrer-Policy'] = 'no-referrer'
        return response

    def limit(request, bucket, count, window):
        address = (request.headers.get('x-real-ip') if trust_proxy else None) or (request.client.host if request.client else 'unknown')
        queue, now = attempts[(bucket, address)], time.time()
        while queue and queue[0] < now - window:
            queue.popleft()
        if len(queue) >= count:
            raise HTTPException(429, 'Too many attempts; try again later')
        queue.append(now)

    def access_token(user, session):
        now = int(time.time())
        return signer.sign({'iss': issuer, 'aud': audience, 'sub': user['id'], 'sid': session['id'],
                            'mirea': user['pulse_uid'], 'grp': user['group_id'], 'iat': now, 'exp': now + 60})

    async def hand_off(user, credentials):
        # The session server keeps MIREA sessions; give it this fresh one, signed by AA ID
        # so it knows whose session it is. Nothing is kept here.
        if not credentials:
            return False
        now = int(time.time())
        token = signer.sign({'iss': issuer, 'aud': 'aa-link', 'sub': user['id'], 'mirea': user['pulse_uid'],
                             'grp': user['group_id'], 'iat': now, 'exp': now + 60})
        try:
            response = await http.post(sessions_url + '/internal/aaid/link', headers={'authorization': 'Bearer ' + token},
                                       json={**credentials, 'group_id': user['group_id'], 'group_name': user['group_name']})
        except httpx.HTTPError:
            return False
        return response.status_code == 200

    def grant(user, session, refresh):
        now = int(time.time())
        claims = {'iss': issuer, 'aud': audience, 'sub': user['id'], 'sid': session['id'],
                  'mirea': user['pulse_uid'], 'grp': user['group_id'], 'iat': now, 'exp': now + access_ttl}
        return {'access_token': signer.sign(claims), 'access_expires_at': claims['exp'],
                'refresh_token': refresh, 'session_id': session['id'], 'user': public(user)}

    def public(user):
        return {'id': user['id'], 'group_id': user['group_id'], 'group_name': user['group_name'],
                'created': user['created'], 'verified_at': user['verified_at']}

    def caller(request):
        scheme, _, token = request.headers.get('authorization', '').partition(' ')
        claims = verify(token, {signer.kid: signer.public}, issuer, audience) if scheme.lower() == 'bearer' else None
        # AA ID can see revocations immediately, unlike services verifying offline.
        if claims is None or store.active_session(claims.get('sid', '')) is None:
            raise HTTPException(401, 'Sign in to AA ID', headers={'WWW-Authenticate': 'Bearer'})
        user = store.user(claims['sub'])
        if user is None or user['status'] != 'active':
            raise HTTPException(403, 'This AA ID is blocked')
        return claims, user

    @app.get('/health')
    async def health():
        return {'ok': True, 'service': 'aa-id'}

    @app.get('/.well-known/jwks.json')
    async def jwks():
        return {**signer.jwks(), 'issuer': issuer, 'audience': audience}

    @app.post('/v1/auth/mirea')
    async def sign_in(body: MireaSignIn, request: Request):
        limit(request, 'mirea', 30, 60)
        try:
            uid, group_id, group_name = await pulse.whoami(body.pulse_cookie)
        except ValueError:
            raise HTTPException(400, 'Invalid Pulse session cookie') from None
        except (httpx.HTTPError, OSError):
            raise HTTPException(503, 'MIREA Pulse is unavailable; try again') from None
        if uid is None:
            raise HTTPException(401, 'The MIREA session is not valid; sign in to MIREA again')
        try:
            user = store.bind_mirea(uid, group_id, group_name)
        except Blocked:
            raise HTTPException(403, 'This AA ID is blocked') from None
        device = re.sub(r'[\x00-\x1f\x7f]', '', body.device).strip() or 'Unknown device'
        session, refresh = store.create_session(user['id'], device, session_ttl, max_sessions)
        return grant(user, session, refresh)

    @app.post('/v1/auth/refresh')
    async def refresh(body: RefreshBody, request: Request):
        limit(request, 'refresh', 60, 60)
        session, token = store.rotate(body.refresh_token, session_ttl)
        if session is None:
            raise HTTPException(401, 'Sign in to AA ID again')
        user = store.user(session['user_id'])
        if user is None or user['status'] != 'active':
            raise HTTPException(403, 'This AA ID is blocked')
        return grant(user, session, token)

    @app.post('/v1/auth/logout')
    async def logout(request: Request):
        claims, user = caller(request)
        store.revoke(user['id'], claims['sid'], 'logout')
        return {'ok': True}

    @app.get('/v1/me')
    async def me(request: Request):
        claims, user = caller(request)
        return {**public(user), 'session_id': claims['sid'], 'max_sessions': max_sessions}

    @app.get('/v1/me/sessions')
    async def sessions(request: Request):
        claims, user = caller(request)
        return {'sessions': [{**dict(row), 'current': row['id'] == claims['sid']} for row in store.sessions(user['id'])],
                'max_sessions': max_sessions}

    @app.delete('/v1/me/sessions/{session_id}')
    async def revoke(session_id: str, request: Request):
        _, user = caller(request)
        if not store.revoke(user['id'], session_id, 'owner'):
            raise HTTPException(404, 'Session not found')
        return {'ok': True}

    # The AA ID site: sign in with the MIREA QR code, see the account, manage devices.
    def site_session(request):
        session = store.web_session(request.cookies.get('aaid_web', ''))
        if session is None:
            raise HTTPException(401, 'Войдите в AA ID')
        user = store.user(session['user_id'])
        if user is None or user['status'] != 'active':
            raise HTTPException(403, 'Этот AA ID заблокирован')
        return session, user

    def login_job(identifier, request):
        job = logins.jobs.get(identifier)
        token = request.headers.get('x-login-token', '')
        if job is None or not token or not hmac.compare_digest(digest(token), job.token_hash):
            raise HTTPException(404, 'Вход не найден или истёк')
        return job

    @app.get('/')
    async def site():
        return FileResponse(static / 'index.html')

    @app.post('/web/logins')
    async def site_login(request: Request):
        limit(request, 'web-login', 10, 300)
        try:
            job, token = logins.start(digest)
        except ValueError as error:
            raise HTTPException(503, str(error)) from None
        return {'id': job.id, 'token': token}

    @app.get('/web/logins/{identifier}')
    async def site_login_state(identifier: str, request: Request):
        job = login_job(identifier, request)
        png = logins.qr_png(job)
        return {'status': job.status, 'error': job.error, 'qr': job.qr,
                'qr_png': 'data:image/png;base64,' + base64.b64encode(png).decode() if png else None}

    @app.post('/web/logins/{identifier}/claim')
    async def site_login_claim(identifier: str, request: Request):
        job = login_job(identifier, request)
        if job.status != 'complete' or job.identity is None or job.claimed:
            raise HTTPException(409, 'Вход ещё не завершён')
        job.claimed = True
        uid, group_id, group_name = job.identity
        credentials, job.identity, job.session = job.session, None, None
        try:
            user = store.bind_mirea(uid, group_id, group_name)
        except Blocked:
            raise HTTPException(403, 'Этот AA ID заблокирован') from None
        linked = await hand_off(user, credentials)
        device = browser_name(request.headers.get('user-agent', ''))
        _, token = store.create_session(user['id'], device, web_ttl, 1, kind='web')
        response = JSONResponse({'ok': True, 'linked': linked})
        response.set_cookie('aaid_web', token, max_age=web_ttl, httponly=True, secure=secure_cookie, samesite='strict', path='/')
        return response

    @app.get('/web/me')
    async def site_me(request: Request):
        session, user = site_session(request)
        devices = [{**dict(row), 'current': row['id'] == session['id']} for row in store.sessions(user['id'])]
        return {**public(user), 'sessions': devices, 'max_sessions': max_sessions}

    @app.get('/web/attendance')
    async def site_attendance(request: Request):
        session, user = site_session(request)
        try:
            response = await http.get(sessions_url + '/api/me/attendance',
                                      headers={'authorization': 'Bearer ' + access_token(user, session)})
        except httpx.HTTPError:
            raise HTTPException(503, 'Сервер сессий недоступен') from None
        if response.status_code == 404:
            raise HTTPException(404, 'Ваш аккаунт МИРЭА не подключён к серверу AntiAttendance')
        if response.status_code == 409:
            raise HTTPException(409, 'Сессия МИРЭА на сервере завершена, войдите на сервере заново')
        if response.status_code != 200:
            raise HTTPException(503, 'Не удалось получить данные турникетов')
        data = response.json()
        return {key: data.get(key) for key in ('inside', 'last_in', 'last_out', 'events')}

    @app.get('/web/schedule')
    async def site_schedule(request: Request):
        _, user = site_session(request)
        group_id = user['group_id']
        if not group_id:
            return {'group_id': None, 'url': schedule.HOST, 'current': None, 'next': None}
        cached = calendars.get(group_id)
        if cached is None or time.time() - cached[0] > 900:
            try:
                response = await http.get(schedule.ical_url(group_id))
                if response.status_code != 200 or len(response.content) > 2 * 1024 * 1024 or b'BEGIN:VCALENDAR' not in response.content:
                    raise ValueError('Invalid calendar')
                cached = calendars[group_id] = (time.time(), response.text)
            except (httpx.HTTPError, ValueError):
                if cached is None:
                    raise HTTPException(503, 'Расписание МИРЭА недоступно') from None
        now = datetime.now(timezone(timedelta(hours=3))).replace(tzinfo=None)  # Moscow civil time
        current, upcoming = schedule.current_and_next(schedule.lessons_on(cached[1], now.date()), now)
        shown = lambda lesson: lesson and {**lesson, 'start': lesson['start'].strftime('%H:%M'), 'end': lesson['end'].strftime('%H:%M')}
        return {'group_id': group_id, 'url': schedule.page_url(group_id), 'current': shown(current), 'next': shown(upcoming)}

    @app.delete('/web/sessions/{session_id}')
    async def site_revoke(session_id: str, request: Request):
        _, user = site_session(request)
        if not store.revoke(user['id'], session_id, 'owner'):
            raise HTTPException(404, 'Устройство не найдено')
        return {'ok': True}

    @app.post('/web/logout')
    async def site_logout(request: Request):
        session = store.web_session(request.cookies.get('aaid_web', ''))
        if session is not None:
            store.revoke(session['user_id'], session['id'], 'logout')
        response = JSONResponse({'ok': True})
        response.delete_cookie('aaid_web', path='/')
        return response

    @app.post('/web/authorize')
    async def site_authorize(body: AuthorizeBody, request: Request):
        # "Sign in with AA ID" on the AntiAttendance site: a one-time code for the
        # signed-in person, sent only to the configured site address.
        _, user = site_session(request)
        if not site_url:
            raise HTTPException(404, 'Вход на сайт AntiAttendance не настроен')
        now = time.time()
        for key, (expires, _) in list(codes.items()):
            if expires < now:
                del codes[key]
        code = secrets.token_urlsafe(32)
        codes[digest(code)] = (now + 60, user['id'])
        return {'redirect': site_url + '/auth/callback?' + urlencode({'code': code, 'state': body.state})}

    @app.post('/v1/site/exchange')
    async def site_exchange(body: CodeBody, request: Request):
        # The site's server trades the code for the person, once.
        limit(request, 'exchange', 60, 60)
        entry = codes.pop(digest(body.code), None)
        if entry is None or entry[0] < time.time():
            raise HTTPException(400, 'Код входа недействителен')
        user = store.user(entry[1])
        if user is None or user['status'] != 'active':
            raise HTTPException(403, 'Этот AA ID заблокирован')
        return {'id': user['id'], 'mirea': user['pulse_uid'], 'group_id': user['group_id'], 'group_name': user['group_name']}

    @app.put('/v1/admin/users/{user_id}/status')
    async def set_status(user_id: str, body: StatusBody, request: Request):
        # Moderation only; disabled unless ADMIN_TOKEN is configured.
        supplied = request.headers.get('x-admin-token', '')
        if len(admin_token) < 24 or not hmac.compare_digest(supplied, admin_token):
            raise HTTPException(403, 'Forbidden')
        if not store.set_status(user_id, body.status):
            raise HTTPException(404, 'User not found')
        return {'ok': True}

    return app

