"""The AntiAttendance site (2.0): sign in with AA ID, see the current lesson, the
groups at it and the people you may mark there, and mark them with the
teacher's QR code.

Who sees whom is decided by the lesson: a signed-in person sees only the people
whose group has the same class right now (same time and room, or for an online
class the same subject and teacher)."""
import asyncio
import hmac
import logging
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

log = logging.getLogger(__name__)
SESSION_COOKIE = 'aa_site'
SESSION_TTL = 30 * 86400
# A mark made this long before the lesson starts still counts for it.
MARK_LEAD = 15 * 60
INSIDE_TTL = 120
INSIDE_WAIT = 2
# Lesson attendance read from the Pulse group journal, per group, at most this often.
MIREA_TTL = 120

class LinkBody(BaseModel):
    cookie: str = Field(min_length=1, max_length=65536)
    storage_state: dict | None = None
    expires_at: float | None = None
    renewed_at: float | None = None
    group_id: int | None = Field(default=None, gt=0, le=2**63 - 1)
    group_name: str | None = Field(default=None, max_length=100)

class MarkBody(BaseModel):
    qr: str = Field(min_length=1, max_length=8192)
    account_ids: list[str] | None = Field(default=None, min_length=1, max_length=200)
    # Mark the person's own group whatever the schedule says (no lesson found,
    # schedule unavailable, another room): everyone in it with a working session.
    own_group: bool = False

class ShareBody(BaseModel):
    account_id: str = Field(min_length=1, max_length=64)

class SettingsBody(BaseModel):
    label: str | None = Field(default=None, min_length=1, max_length=60)
    pass_allowed: bool | None = None

def shown(lesson):
    return lesson and {key: lesson[key] for key in ('subject', 'type', 'location', 'teacher')} | {
        'start': lesson['start'].strftime('%H:%M'), 'end': lesson['end'].strftime('%H:%M'),
        'starts_at': schedule.epoch(lesson['start']), 'ends_at': schedule.epoch(lesson['end'])}

def install(app, service, aaid, calendars, static):
    vault = service.vault
    secure = os.environ.get('PUBLIC_URL', '').startswith('https://')
    attempts = defaultdict(deque)
    inside_cache = {}
    inside_checks = {}
    pulse_slots = asyncio.Semaphore(4)
    app.state.presence = {'cache': inside_cache, 'checks': inside_checks, 'ttl': INSIDE_TTL}
    mirea_days, mirea_tasks = {}, {}
    app.state.mirea = mirea_days

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

    @app.get('/today')
    async def today_page():
        return FileResponse(static / 'today.html')

    @app.get('/tg/connect')
    async def telegram_page():
        return FileResponse(static / 'telegram.html')

    @app.post('/site/telegram-link')
    async def telegram_link(request: Request):
        """A one-time Telegram deep link that binds the chat opening it to this AA ID.
        The code is made here, by the signed-in person, so nobody can bind their
        own chat to someone else's account by sending them a link."""
        session = person(request)
        limit(('telegram-link', session['aaid_id']), 10, 600)
        bot = service.telegram
        if bot is None or not bot.username:
            raise HTTPException(503, 'Бот Telegram сейчас недоступен')
        own = vault.account_by_pulse(session['mirea'])
        if own is None:
            raise HTTPException(409, 'Ваш аккаунт МИРЭА не подключён к серверу. Войдите через AA ID заново по QR-коду.')
        code = vault.telegram_link_code(session['aaid_id'], own['id'])
        return {'url': f'https://t.me/{bot.username}?start=aaid_{code}', 'expires_in': 600}

    RETURN_PAGES = ('/today', '/tg/connect')

    @app.get('/auth/aaid')
    async def sign_in(fresh: int = 0, next: str = ''):
        if aaid is None or not aaid.public_url:
            raise HTTPException(503, 'AA ID не настроен на сервере')
        state = secrets.token_urlsafe(24)
        query = {'continue': 'aa', 'state': state, **({'fresh': '1'} if fresh else {})}
        response = RedirectResponse(aaid.public_url.rstrip('/') + '/?' + urlencode(query), status_code=303)
        response.set_cookie('aa_state', state, max_age=900, httponly=True, secure=secure, samesite='lax', path='/auth')
        if next in RETURN_PAGES:
            response.set_cookie('aa_next', next, max_age=900, httponly=True, secure=secure, samesite='lax', path='/auth')
        return response

    @app.get('/auth/callback')
    async def callback(request: Request, code: str = '', state: str = ''):
        expected = request.cookies.get('aa_state', '')
        person_data = None
        if aaid is not None and 20 <= len(code) <= 200 and expected and hmac.compare_digest(state, expected):
            person_data = await aaid.exchange(code)
        if person_data is None:
            log.warning('AA ID sign-in failed: %s', 'code not accepted' if expected and hmac.compare_digest(state, expected) else 'state cookie missing or different')
            return RedirectResponse('/?signin=failed', status_code=303)
        token = vault.create_site_session(person_data['id'], person_data['mirea'], person_data.get('group_id'),
                                          person_data.get('group_name'), SESSION_TTL)
        own = vault.account_by_pulse(person_data['mirea'])
        if own is not None and own['group_id'] is None and person_data.get('group_id'):
            # Without a group the person is not shown at any lesson; AA ID knows it.
            vault.update_group(own['id'], person_data['group_id'], person_data.get('group_name'), own['version'])
            await service.changed()
        back = request.cookies.get('aa_next', '')
        response = RedirectResponse(back if back in RETURN_PAGES else '/', status_code=303)
        response.delete_cookie('aa_state', path='/auth')
        response.delete_cookie('aa_next', path='/auth')
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
            log.warning('AA ID link rejected: bad signature')
            raise HTTPException(401, 'Invalid AA ID link')
        try:
            cookie = validate_header(body.cookie)
        except ValueError:
            raise HTTPException(400, 'Invalid cookie') from None
        state, uid = await service.transport.identity(cookie)
        if state != 'valid' or uid != claims['mirea']:
            log.warning('AA ID link rejected: Pulse says %s', state if state != 'valid' else 'another person')
            raise HTTPException(422, 'Pulse did not confirm this session')
        credentials = {'cookie': cookie, 'expires_at': body.expires_at, 'renewed_at': body.renewed_at or time.time()}
        if body.storage_state:
            credentials['storage_state'] = body.storage_state
        existing = vault.account_by_pulse(uid)
        keep_group = existing is not None and existing['group_id'] is not None
        account = vault.upsert(uid, None if existing else 'Без имени', credentials,
                               group_id=None if keep_group else body.group_id,
                               group_name=None if keep_group else body.group_name)
        if 'usr' in claims:
            vault.set_username(account['id'], claims['usr'])
        vault.audit('aaid_linked', 'aaid:' + claims['sub'], account['id'])
        await service.changed()
        return {'account_id': account['id']}

    async def check_inside(row, cookie):
        start = (int((time.time() + 3 * 3600) // 86400)) * 86400 - 3 * 3600
        async with pulse_slots:
            try:
                result = await service.transport.acs_timeline(cookie, row['pulse_id'], start, start + 86400 - 1)
            except Exception:
                result = None
        inside_cache[row['id']] = (time.time(), None if result is None else result[1])
        inside_checks.pop(row['id'], None)

    async def inside(row):
        # Whether the person is in the university now: the direction of today's last
        # turnstile pass. None when unknown (no session, Pulse unavailable). A slow
        # Pulse never holds the page: past INSIDE_WAIT the last known answer is shown and
        # the check finishes in the background.
        cached = inside_cache.get(row['id'])
        if cached and time.time() - cached[0] < INSIDE_TTL:
            return cached[1]
        cookie = vault.secret(row).get('cookie')
        if not cookie or row['state'] != 'valid':
            return None
        check = inside_checks.get(row['id'])
        if check is None:
            check = inside_checks[row['id']] = asyncio.ensure_future(check_inside(row, cookie))
        try:
            await asyncio.wait_for(asyncio.shield(check), INSIDE_WAIT)
        except asyncio.TimeoutError:
            return cached[1] if cached else None
        return inside_cache[row['id']][1]

    async def read_journal(group_id, day):
        # One working account of the group reads the whole group's journal: its
        # current semester journal, the day's lessons, and everyone's status at each
        # lesson that has begun. Lessons read well after they ended are not re-read.
        transport = service.transport
        previous = mirea_days.get(group_id)
        previous = previous if previous and previous['day'] == day else None
        known = {lesson['id']: lesson for lesson in previous['lessons']} if previous else {}
        entry = {'day': day, 'fetched': time.time(), 'lessons': [], 'error': 'Нет подключённых аккаунтов группы'}
        try:
            for row in [row for row in vault.accounts(group_id) if row['state'] == 'valid'][:2]:
                cookie = vault.secret(row).get('cookie')
                if not cookie:
                    continue
                async with pulse_slots:
                    journals = await transport.visiting_logs(cookie)
                    lessons = await transport.day_lessons(cookie, day) if journals else None
                if not journals or lessons is None:
                    entry['error'] = 'Пульс не отдал журнал группы'
                    continue
                now = time.time()
                journal = next((j for j in journals if j['start'] and j['end'] and j['start'] <= now <= j['end']), journals[0])
                async def status(lesson):
                    old = known.get(lesson['id'])
                    if old and old['status'] and lesson['end'] < previous['fetched'] - 3600:
                        return old['status']
                    if lesson['start'] > now + MARK_LEAD:
                        return {}
                    async with pulse_slots:
                        return await transport.lesson_attendance(cookie, lesson['id'], journal['id']) or {}
                found = await asyncio.gather(*(status(lesson) for lesson in lessons))
                entry.update(lessons=[lesson | {'status': got} for lesson, got in zip(lessons, found)], error=None, journal=journal['group'])
                break
        except Exception as error:
            log.warning('Pulse journal for group %s failed: %s: %s', group_id, type(error).__name__, error)
            entry['error'] = 'Ошибка чтения журнала Пульса'
        finally:
            mirea_days[group_id] = entry
            mirea_tasks.pop(group_id, None)
        if entry['error']:
            log.info('Pulse journal for group %s: %s', group_id, entry['error'])

    async def journals(group_ids):
        """Today's Pulse journal of each group, refreshed in the background when stale.
        A first read is waited for briefly; after that the page never waits for Pulse."""
        if not hasattr(service.transport, 'lesson_attendance'):
            return {}
        day = schedule.moscow_now().date()
        waiting = []
        for gid in set(filter(None, group_ids)):
            entry = mirea_days.get(gid)
            if entry and entry['day'] == day and time.time() - entry['fetched'] < MIREA_TTL:
                continue
            task = mirea_tasks.get(gid)
            if task is None:
                task = mirea_tasks[gid] = asyncio.ensure_future(read_journal(gid, day))
            if entry is None or entry['day'] != day:
                waiting.append(task)
        if waiting:
            await asyncio.wait([asyncio.shield(task) for task in waiting], timeout=INSIDE_WAIT)
        return {gid: entry for gid, entry in mirea_days.items() if entry['day'] == day}

    def mirea_status(found, row, lesson):
        # The person's status at our schedule's lesson, matched to Pulse's by start time.
        entry = found.get(row['group_id'])
        start = schedule.epoch(lesson['start'])
        for theirs in entry['lessons'] if entry else ():
            if abs(theirs['start'] - start) <= 600:
                return theirs['status'].get(row['pulse_id'])
        return None

    async def lesson_context(session):
        """The person's current lesson, the groups at it and the accounts in them."""
        own = vault.account_by_pulse(session['mirea'])
        group_id = session['group_id'] or (own['group_id'] if own else None)
        group_name = session['group_name'] or (own['group_name'] if own else None)
        now = schedule.moscow_now()
        context = {'own': own, 'group_id': group_id, 'group_name': group_name, 'current': None, 'next': None,
                   'active': None, 'had_lessons': False, 'groups': [], 'accounts': [], 'schedule_error': None}
        if not group_id:
            return context
        lessons = await calendars.lessons(group_id, now.date())
        if lessons is None:
            context['schedule_error'] = 'Расписание МИРЭА недоступно'
            return context
        current, upcoming = schedule.current_and_next(lessons, now)
        # Shortly before a lesson its people are already shown, and marks count for it.
        soon = upcoming if upcoming and (upcoming['start'] - now).total_seconds() <= MARK_LEAD else None
        context['current'], context['next'], context['active'] = current, upcoming, current or soon
        context['had_lessons'] = any(lesson['end'] <= now for lesson in lessons)
        current = current or soon
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

    @app.get('/site/today')
    async def today(request: Request):
        """Today's lessons of the person's group with everyone's marks. Another group
        joins a lesson's column only when it was at that same class."""
        session = person(request)
        own = vault.account_by_pulse(session['mirea'])
        group_id = session['group_id'] or (own['group_id'] if own else None)
        group_name = session['group_name'] or (own['group_name'] if own else None)
        now = schedule.moscow_now()
        result = {'date': now.strftime('%d.%m.%Y'), 'group_id': group_id, 'group_name': group_name,
                  'lessons': [], 'people': [], 'schedule_error': None}
        if not group_id:
            return result
        lessons = await calendars.lessons(group_id, now.date())
        if lessons is None:
            result['schedule_error'] = 'Расписание МИРЭА недоступно'
            return result
        known = {row['id']: row['name'] for row in vault.groups()}
        known.setdefault(group_id, group_name)
        others = [gid for gid in known if gid != group_id]
        found = dict(zip(others, await asyncio.gather(*(calendars.lessons(gid, now.date()) for gid in others))))
        columns = []
        for lesson in lessons:
            groups = [group_id] + [gid for gid in others if found[gid] and any(schedule.same_lesson(lesson, theirs) for theirs in found[gid])]
            columns.append((lesson, groups))
        present = {gid for _, groups in columns for gid in groups} | {group_id}
        rows = [row for row in vault.accounts() if row['group_id'] in present]
        start_of_day = schedule.epoch(now.replace(hour=0, minute=0, second=0, microsecond=0))
        marks = vault.marks_since([row['id'] for row in rows], start_of_day - MARK_LEAD)
        found = await journals(present)
        result['mirea'] = {str(gid): {'fetched': entry['fetched'], 'error': entry['error']} for gid, entry in found.items() if gid in present}
        for lesson, groups in columns:
            begins, ends = schedule.epoch(lesson['start']) - MARK_LEAD, schedule.epoch(lesson['end'])
            # A mark counts for the lesson it was made around; an approval wins over later failures.
            cells = {}
            for mark in marks:
                if begins <= mark['created'] < ends and (cells.get(mark['account_id'], {}).get('state') != 'approved'):
                    cells[mark['account_id']] = {key: mark[key] for key in ('state', 'message', 'created', 'source')}
            state = 'past' if lesson['end'] <= now else 'now' if lesson['start'] <= now else 'future'
            result['lessons'].append(shown(lesson) | {'state': state, 'groups': [{'id': gid, 'name': known.get(gid)} for gid in groups],
                                                      'marks': {row['id']: cells.get(row['id']) for row in rows if row['group_id'] in groups},
                                                      'mirea': {row['id']: mirea_status(found, row, lesson) for row in rows if row['group_id'] in groups}})
        result['people'] = sorted(({'id': row['id'], 'label': row['label'], 'username': row['username'], 'group_id': row['group_id'],
                                    'group_name': row['group_name'] or known.get(row['group_id']), 'session': row['state'],
                                    'own': own is not None and row['id'] == own['id']} for row in rows),
                                  key=lambda p: (p['group_id'] != group_id, p['group_name'] or '', p['label'].casefold()))
        return result

    @app.get('/site/state')
    async def state(request: Request):
        session = person(request)
        context = await lesson_context(session)
        own, current = context['own'], context['active']
        people = []
        if current is not None:
            rows = context['accounts']
            marks = vault.latest_marks([row['id'] for row in rows], schedule.epoch(current['start']) - MARK_LEAD)
            present, found = await asyncio.gather(asyncio.gather(*(inside(row) for row in rows)),
                                                  journals(group['id'] for group in context['groups']))
            names = {group['id']: group['name'] for group in context['groups']}
            for row, here in zip(rows, present):
                mark = marks.get(row['id'])
                people.append({'id': row['id'], 'label': row['label'], 'username': row['username'], 'group_id': row['group_id'],
                               'group_name': row['group_name'] or names.get(row['group_id']),
                               'own': own is not None and row['id'] == own['id'], 'session': row['state'],
                               'inside': here, 'mark': mark and {key: mark[key] for key in ('state', 'message', 'created')},
                               'mirea': mirea_status(found, row, current)})
            people.sort(key=lambda p: (not p['own'], p['group_id'] != context['group_id'], p['label'].casefold()))
        return {'me': {'group_id': context['group_id'], 'group_name': context['group_name'],
                       'account_id': own['id'] if own else None, 'label': own['label'] if own else None,
                       'connected': bool(own and own['state'] == 'valid')},
                'lesson': shown(context['current']), 'next': shown(context['next']),
                'active': None if current is None else 'now' if context['current'] else 'soon',
                'had_lessons': context['had_lessons'],
                'own_group_size': sum(1 for row in vault.accounts(context['group_id']) if row['state'] == 'valid') if context['group_id'] else 0,
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
        if body.own_group:
            if not context['group_id']:
                raise HTTPException(409, 'Группа не определена')
            allowed = {row['id']: row for row in vault.accounts(context['group_id'])}
            chosen = list(dict.fromkeys(body.account_ids)) if body.account_ids else [
                identifier for identifier, row in allowed.items() if row['state'] == 'valid']
            if not chosen:
                raise HTTPException(409, 'В вашей группе нет подключённых людей')
        else:
            if context['active'] is None:
                raise HTTPException(409, 'Сейчас нет пары')
            if not body.account_ids:
                raise HTTPException(422, 'Выберите людей')
            allowed = {row['id']: row for row in context['accounts']}
            chosen = list(dict.fromkeys(body.account_ids))
        if any(identifier not in allowed for identifier in chosen):
            raise HTTPException(403, 'Отмечать можно только людей на вашей паре' if not body.own_group else 'Отмечать можно только свою группу')
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
            results.append({'id': identifier, 'label': allowed[identifier]['label'], 'state': result, 'message': message})
        return {'results': results}

    # The person's own settings, changed on the AA ID site (AA ID token, /api/me/).
    def own_account(request):
        caller = request.state.aaid
        if caller is None:
            raise HTTPException(401, 'AA ID sign-in required')
        own = vault.account_by_pulse(caller['mirea'])
        # AA ID owns the username; its signed token carries the current one.
        if own is not None and 'usr' in caller and own['username'] != caller['usr']:
            vault.set_username(own['id'], caller['usr'])
            own = vault.account(own['id'])
        return own

    def require_own(request):
        own = own_account(request)
        if own is None:
            raise HTTPException(404, 'This MIREA account is not connected to the session server')
        return own

    def settings(own):
        return {'connected': own is not None and own['state'] == 'valid', 'account': own is not None,
                'label': own['label'] if own else None, 'pass_allowed': bool(own['pass_allowed']) if own else None,
                'group_name': own['group_name'] if own else None}

    def card(row):
        return {'account_id': row['id'], 'username': row['username'], 'label': row['label'], 'group_name': row['group_name']}

    def pass_list(own):
        people = [{**card(row), 'kind': 'aaid', 'id': row['id']} for row in vault.pass_shares(own['id'])]
        people += [{'kind': 'telegram', 'id': grant['id'], 'account_id': None, 'username': None,
                    'label': grant['label'], 'group_name': None} for grant in service.agreements.granted(own['id'])]
        return {'pass_allowed': bool(own['pass_allowed']), 'people': people}

    @app.get('/api/me/people')
    async def find_people(request: Request, q: str = ''):
        own = own_account(request)
        limit(('people', request.state.aaid['sub']), 60, 60)
        return {'people': [card(row) for row in vault.search_people(q[:60], own['id'] if own else None)]}

    @app.get('/api/me/pass')
    async def my_pass(request: Request):
        return pass_list(require_own(request))

    @app.post('/api/me/pass/people')
    async def share(body: ShareBody, request: Request):
        own = require_own(request)
        grantee = vault.account(body.account_id)
        if grantee is None or grantee['id'] == own['id']:
            raise HTTPException(404, 'Человек не найден')
        vault.share_pass(own['id'], grantee['id'])
        vault.audit('pass_shared', 'aaid:' + request.state.aaid['sub'], own['id'])
        await service.changed()
        return pass_list(vault.account(own['id']))

    @app.delete('/api/me/pass/people/{kind}/{identifier}')
    async def unshare(kind: str, identifier: str, request: Request):
        own = require_own(request)
        actor = 'aaid:' + request.state.aaid['sub']
        if kind == 'aaid':
            removed = vault.unshare_pass(own['id'], identifier)
        elif kind == 'telegram':
            removed = service.agreements.revoke_by_owner_account(identifier, own['id'], actor)
        else:
            removed = False
        if not removed:
            raise HTTPException(404, 'Такого разрешения нет')
        if kind == 'aaid':
            vault.audit('pass_unshared', actor, own['id'])
        await service.changed()
        return pass_list(vault.account(own['id']))

    @app.get('/api/me/account')
    async def my_account(request: Request):
        return settings(own_account(request))

    @app.put('/api/me/account')
    async def change_my_account(body: SettingsBody, request: Request):
        own = require_own(request)
        if body.label is not None:
            try:
                vault.rename(own['id'], body.label)
            except ValueError:
                raise HTTPException(400, 'Invalid name') from None
        if body.pass_allowed is not None:
            vault.set_pass_allowed(own['id'], body.pass_allowed)
        await service.changed()
        return settings(vault.account(own['id']))
