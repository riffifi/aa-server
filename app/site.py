"""The AntiAttendance site (2.0): sign in with AA ID, see the current lesson, the
groups at it and the people you may mark there, and mark them with the
teacher's QR code.

Who sees whom is decided by the lesson: a signed-in person sees only the people
whose group has the same class right now (same time and room, or for an online
class the same subject and teacher)."""
import asyncio
import hmac
import os
import secrets
import time
from collections import defaultdict, deque
from urllib.parse import urlencode
from fastapi import HTTPException, Request
from fastapi.responses import FileResponse, JSONResponse, RedirectResponse
from pydantic import BaseModel, Field
from . import protocol, schedule
from .pulse import validate_header

SESSION_COOKIE = 'aa_site'
SESSION_TTL = 30 * 86400
# A mark made this long before the lesson starts still counts for it.
MARK_LEAD = 15 * 60
INSIDE_TTL = 120

class LinkBody(BaseModel):
    cookie: str = Field(min_length=1, max_length=65536)
    storage_state: dict | None = None
    expires_at: float | None = None
    renewed_at: float | None = None
    group_id: int | None = Field(default=None, gt=0, le=2**63 - 1)
    group_name: str | None = Field(default=None, max_length=100)

class MarkBody(BaseModel):
    qr: str = Field(min_length=1, max_length=8192)
    account_ids: list[str] = Field(min_length=1, max_length=200)

class NameBody(BaseModel):
    label: str = Field(min_length=1, max_length=60)

def shown(lesson):
    return lesson and {key: lesson[key] for key in ('subject', 'type', 'location', 'teacher')} | {
        'start': lesson['start'].strftime('%H:%M'), 'end': lesson['end'].strftime('%H:%M')}

def install(app, service, aaid, calendars, static):
    vault = service.vault
    secure = os.environ.get('PUBLIC_URL', '').startswith('https://')
    attempts = defaultdict(deque)
    inside_cache = {}
    pulse_slots = asyncio.Semaphore(4)

    def limit(key, count, window):
        queue, now = attempts[key], time.time()
        while queue and queue[0] < now - window:
            queue.popleft()
        if len(queue) >= count:
            raise HTTPException(429, 'Слишком часто, подождите немного')
        queue.append(now)

    def person(request):
        session = vault.site_session(request.cookies.get(SESSION_COOKIE, ''))
        if session is None:
            raise HTTPException(401, 'Войдите через AA ID')
        return session

    @app.get('/')
    async def index():
        return FileResponse(static / 'index.html')

    @app.get('/auth/aaid')
    async def sign_in(fresh: int = 0):
        if aaid is None or not aaid.public_url:
            raise HTTPException(503, 'AA ID не настроен на сервере')
        state = secrets.token_urlsafe(24)
        query = {'continue': 'aa', 'state': state, **({'fresh': '1'} if fresh else {})}
        response = RedirectResponse(aaid.public_url.rstrip('/') + '/?' + urlencode(query), status_code=303)
        response.set_cookie('aa_state', state, max_age=900, httponly=True, secure=secure, samesite='lax', path='/auth')
        return response

    @app.get('/auth/callback')
    async def callback(request: Request, code: str = '', state: str = ''):
        expected = request.cookies.get('aa_state', '')
        person_data = None
        if aaid is not None and 20 <= len(code) <= 200 and expected and hmac.compare_digest(state, expected):
            person_data = await aaid.exchange(code)
        if person_data is None:
            return RedirectResponse('/?signin=failed', status_code=303)
        token = vault.create_site_session(person_data['id'], person_data['mirea'], person_data.get('group_id'),
                                          person_data.get('group_name'), SESSION_TTL)
        response = RedirectResponse('/', status_code=303)
        response.delete_cookie('aa_state', path='/auth')
        response.set_cookie(SESSION_COOKIE, token, max_age=SESSION_TTL, httponly=True, secure=secure, samesite='strict', path='/')
        return response

    @app.post('/site/logout')
    async def sign_out(request: Request):
        vault.delete_site_session(request.cookies.get(SESSION_COOKIE, ''))
        response = JSONResponse({'ok': True})
        response.delete_cookie(SESSION_COOKIE, path='/')
        return response

    @app.post('/internal/aaid/link')
    async def link(body: LinkBody, request: Request):
        # AA ID hands over the MIREA session from a fresh QR sign-in, signed with its
        # key, so an AA ID sign-in also connects (or reconnects) the person here.
        scheme, _, token = request.headers.get('authorization', '').partition(' ')
        claims = await aaid.verify(token, audience='aa-link') if aaid is not None and scheme.lower() == 'bearer' else None
        if claims is None:
            raise HTTPException(401, 'Invalid AA ID link')
        try:
            cookie = validate_header(body.cookie)
        except ValueError:
            raise HTTPException(400, 'Invalid cookie') from None
        state, uid = await service.transport.identity(cookie)
        if state != 'valid' or uid != claims['mirea']:
            raise HTTPException(422, 'Pulse did not confirm this session')
        credentials = {'cookie': cookie, 'expires_at': body.expires_at, 'renewed_at': body.renewed_at or time.time()}
        if body.storage_state:
            credentials['storage_state'] = body.storage_state
        existing = vault.account_by_pulse(uid)
        keep_group = existing is not None and existing['group_id'] is not None
        account = vault.upsert(uid, None if existing else 'Без имени', credentials,
                               group_id=None if keep_group else body.group_id,
                               group_name=None if keep_group else body.group_name)
        vault.audit('aaid_linked', 'aaid:' + claims['sub'], account['id'])
        await service.changed()
        return {'account_id': account['id']}

    async def inside(row):
        # Whether the person is in the university now: the direction of today's last
        # turnstile pass. None when unknown (no session, Pulse unavailable).
        cached = inside_cache.get(row['id'])
        if cached and time.time() - cached[0] < INSIDE_TTL:
            return cached[1]
        cookie = vault.secret(row).get('cookie')
        if not cookie or row['state'] != 'valid':
            return None
        start = (int((time.time() + 3 * 3600) // 86400)) * 86400 - 3 * 3600
        async with pulse_slots:
            try:
                result = await service.transport.acs_timeline(cookie, row['pulse_id'], start, start + 86400 - 1)
            except Exception:
                result = None
        value = None if result is None else result[1]
        inside_cache[row['id']] = (time.time(), value)
        return value

    async def lesson_context(session):
        """The person's current lesson, the groups at it and the accounts in them."""
        own = vault.account_by_pulse(session['mirea'])
        group_id = session['group_id'] or (own['group_id'] if own else None)
        group_name = session['group_name'] or (own['group_name'] if own else None)
        now = schedule.moscow_now()
        context = {'own': own, 'group_id': group_id, 'group_name': group_name, 'current': None, 'next': None,
                   'groups': [], 'accounts': [], 'schedule_error': None}
        if not group_id:
            return context
        lessons = await calendars.lessons(group_id, now.date())
        if lessons is None:
            context['schedule_error'] = 'Расписание МИРЭА недоступно'
            return context
        current, upcoming = schedule.current_and_next(lessons, now)
        context['current'], context['next'] = current, upcoming
        if current is None:
            return context
        known = {row['id']: row['name'] for row in vault.groups()}
        known.setdefault(group_id, group_name)
        others = [gid for gid in known if gid != group_id]
        found = await asyncio.gather(*(calendars.lessons(gid, now.date()) for gid in others))
        groups = [{'id': group_id, 'name': group_name or known.get(group_id), 'own': True}]
        for gid, their in zip(others, found):
            if their and any(schedule.same_lesson(current, lesson) for lesson in their):
                groups.append({'id': gid, 'name': known[gid], 'own': False})
        context['groups'] = groups
        ids = {group['id'] for group in groups}
        context['accounts'] = [row for row in vault.accounts() if row['group_id'] in ids]
        return context

    @app.get('/site/state')
    async def state(request: Request):
        session = person(request)
        context = await lesson_context(session)
        own, current = context['own'], context['current']
        people = []
        if current is not None:
            rows = context['accounts']
            marks = vault.latest_marks([row['id'] for row in rows], schedule.epoch(current['start']) - MARK_LEAD)
            present = await asyncio.gather(*(inside(row) for row in rows))
            names = {group['id']: group['name'] for group in context['groups']}
            for row, here in zip(rows, present):
                mark = marks.get(row['id'])
                people.append({'id': row['id'], 'label': row['label'], 'group_id': row['group_id'],
                               'group_name': row['group_name'] or names.get(row['group_id']),
                               'own': own is not None and row['id'] == own['id'], 'session': row['state'],
                               'inside': here, 'mark': mark and {key: mark[key] for key in ('state', 'message', 'created')}})
            people.sort(key=lambda p: (not p['own'], p['group_id'] != context['group_id'], p['label'].casefold()))
        return {'me': {'group_id': context['group_id'], 'group_name': context['group_name'],
                       'account_id': own['id'] if own else None, 'label': own['label'] if own else None,
                       'connected': bool(own and own['state'] == 'valid')},
                'lesson': shown(current), 'next': shown(context['next']),
                'schedule_url': schedule.page_url(context['group_id']) if context['group_id'] else schedule.HOST,
                'schedule_error': context['schedule_error'],
                'groups': context['groups'], 'people': people, 'aaid_url': aaid.public_url if aaid else None}

    @app.post('/site/mark')
    async def mark(body: MarkBody, request: Request):
        session = person(request)
        limit(('mark', session['aaid_id']), 60, 60)
        try:
            token = protocol.attendance_token(body.qr)
        except ValueError:
            raise HTTPException(400, 'Это не QR-код посещаемости Пульса') from None
        context = await lesson_context(session)
        if context['current'] is None:
            raise HTTPException(409, 'Сейчас нет пары')
        allowed = {row['id']: row for row in context['accounts']}
        chosen = list(dict.fromkeys(body.account_ids))
        if any(identifier not in allowed for identifier in chosen):
            raise HTTPException(403, 'Отмечать можно только людей на вашей паре')
        request_body = protocol.frame(protocol.field(1, token.encode()))

        async def one(row):
            cookie = vault.secret(row).get('cookie')
            if not cookie:
                return 'expired', 'Сессия МИРЭА завершена'
            try:
                reply = await service.transport.rpc(cookie, 'attendance', request_body)
            except Exception:
                return 'error', 'Пульс недоступен'
            return protocol.approval(reply.status, reply.headers, reply.body)

        outcomes = await asyncio.gather(*(one(allowed[identifier]) for identifier in chosen))
        results = []
        for identifier, (result, message) in zip(chosen, outcomes):
            vault.record_mark(identifier, result, message, 'site:' + session['aaid_id'])
            results.append({'id': identifier, 'state': result, 'message': message})
        return {'results': results}

    @app.put('/site/me/name')
    async def rename(body: NameBody, request: Request):
        session = person(request)
        own = vault.account_by_pulse(session['mirea'])
        if own is None:
            raise HTTPException(404, 'Ваш аккаунт МИРЭА ещё не подключён')
        try:
            account = vault.rename(own['id'], body.label)
        except ValueError:
            raise HTTPException(400, 'Неверное имя') from None
        await service.changed()
        return {'label': account['label']}
