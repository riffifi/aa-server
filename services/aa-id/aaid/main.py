import os
import re
import time
from collections import defaultdict, deque
from contextlib import asynccontextmanager
import hmac
import httpx
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse
from pydantic import BaseModel, Field
from .pulse import PulseIdentity
from .store import Store, Blocked
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

class StatusBody(BaseModel):
    status: str = Field(pattern='^(active|blocked)$')

def create_app(directory=None, identity=None):
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
    attempts = defaultdict(deque)

    @asynccontextmanager
    async def lifespan(app):
        yield
        await pulse.close()
        store.close()

    app = FastAPI(title='AA ID', docs_url=None, redoc_url=None, openapi_url=None, lifespan=lifespan)
    app.state.store, app.state.signer = store, signer

    @app.middleware('http')
    async def headers(request, call_next):
        size = request.headers.get('content-length')
        if size and (not size.isdigit() or int(size) > 100000):
            return JSONResponse({'detail': 'Request too large'}, status_code=413)
        response = await call_next(request)
        response.headers['Cache-Control'] = 'no-store'
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

