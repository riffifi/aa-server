import time
from datetime import datetime, timedelta
import pytest
from fastapi.testclient import TestClient
from app.main import create_app
from app.aaid import Verifier
from app.pulse import Reply
from app import protocol, schedule
from test_server import FakePulse, PASSWORD, HEADERS, reply
from test_aaid import RAW, token

NOW = schedule.moscow_now()
START = (NOW - timedelta(minutes=10)).strftime('%Y%m%dT%H%M00')
END = (NOW + timedelta(minutes=80)).strftime('%Y%m%dT%H%M00')

def calendar(subject, room, teacher='Иванов И.И.', start=START, end=END):
    return f'''BEGIN:VCALENDAR\r
BEGIN:VEVENT\r
DTSTART;TZID=Europe/Moscow:{start}\r
DTEND;TZID=Europe/Moscow:{end}\r
SUMMARY:{subject}\r
X-META-DISCIPLINE:{subject}\r
X-META-LESSON_TYPE:ПР\r
X-META-TEACHER:{teacher}\r
LOCATION:{room}\r
END:VEVENT\r
END:VCALENDAR\r
'''

CALENDARS = {
    1: calendar('Физика', 'А-101'),
    2: calendar('Физика', 'А-101'),           # the same class: same time and room
    3: calendar('Физика', 'Б-202'),           # another room at the same time
    4: calendar('Физика', 'СДО'),             # online, same subject and teacher as 5
    5: calendar('Физика', 'СДО'),
}

class LessonPulse(FakePulse):
    journal_reads = 0
    async def acs_timeline(self, cookie, uid, start, end):
        return [], uid == 'ann'
    async def visiting_logs(self, cookie):
        self.journal_reads += 1
        return [{'id': 'old', 'group': 'ГР', 'start': 0, 'end': 1}, {'id': 'now', 'group': 'ГР', 'start': 1, 'end': 4_000_000_000}]
    async def day_lessons(self, cookie, day):
        start = schedule.epoch(datetime.strptime(START, '%Y%m%dT%H%M00'))
        return [{'id': 'L1', 'start': start, 'end': start + 5400, 'subject': 'Физика'}]
    async def lesson_attendance(self, cookie, lesson_id, log_id):
        assert (lesson_id, log_id) == ('L1', 'now')
        return {'bob': 'present', 'ann': 'absent', 'cat': 'excused'}

class Link(Verifier):
    async def exchange(self, code):
        group = (1, 'ГР-1') if code.startswith('fox') else (None, None)
        return {'id': 'aaid-' + code[:3], 'mirea': code[:3], 'group_id': group[0], 'group_name': group[1]} if code.endswith('x' * 17) else None

@pytest.fixture
def setup(tmp_path):
    pulse = LessonPulse()
    async def fetch(url):
        return CALENDARS.get(int(url.split('/Group/')[1].split('?')[0]))
    app = create_app(tmp_path, PASSWORD, pulse, background=False,
                     aaid=Link(keys={'k1': RAW}, public_url='https://id.example', enforce=False), calendar_fetch=fetch)
    vault = app.state.service.vault
    for uid, name, group in (('ann', 'Анна', 1), ('bob', 'Борис', 2), ('cat', 'Катя', 3), ('dan', 'Даня', 4), ('eva', 'Ева', 5)):
        vault.upsert(uid, name, {'cookie': 'session=' + uid}, group_id=group, group_name=f'ГР-{group}')
    with TestClient(app) as client:
        yield client, app, pulse

def sign_in(client, uid='ann'):
    start = client.get('/auth/aaid', follow_redirects=False)
    assert start.status_code == 303 and start.headers['location'].startswith('https://id.example/?continue=aa&state=')
    state = start.headers['location'].split('state=')[1]
    done = client.get(f'/auth/callback?code={uid}{"x" * 17}&state={state}', follow_redirects=False)
    assert done.status_code == 303 and done.headers['location'] == '/'
    assert 'samesite=strict' in done.headers['set-cookie'].lower()

def ids(client, people):
    names = {p['label']: p['id'] for p in client.get('/site/state').json()['people']}
    return [names[name] for name in people]

def test_sign_in_needs_matching_state(setup):
    client, _, _ = setup
    assert client.get('/site/state').status_code == 401
    assert 'Вход через AA ID' in client.get('/').text
    client.get('/auth/aaid', follow_redirects=False)
    failed = client.get('/auth/callback?code=ann' + 'x' * 17 + '&state=forged', follow_redirects=False)
    assert failed.headers['location'] == '/?signin=failed'
    assert client.get('/site/state').status_code == 401

def test_lesson_groups_and_people(setup):
    client, _, _ = setup
    sign_in(client)
    state = client.get('/site/state').json()
    assert state['lesson']['subject'] == 'Физика' and state['lesson']['location'] == 'А-101'
    assert state['me']['group_id'] == 1 and state['me']['connected'] is True
    assert [(g['id'], g['own']) for g in state['groups']] == [(1, True), (2, False)]
    people = {p['label']: p for p in state['people']}
    assert set(people) == {'Анна', 'Борис'}  # Катя is in another room
    assert people['Анна']['own'] and people['Анна']['inside'] is True and people['Борис']['inside'] is False
    assert people['Борис']['mark'] is None

def test_online_class_matches_by_subject_and_teacher(setup):
    client, _, _ = setup
    sign_in(client, 'dan')
    state = client.get('/site/state').json()
    assert [g['id'] for g in state['groups']] == [4, 5]

def test_mark_only_people_at_the_lesson(setup):
    client, app, pulse = setup
    sign_in(client)
    ann, bob = ids(client, ['Анна', 'Борис'])
    cat = app.state.service.vault.account_by_pulse('cat')['id']
    qr = 'https://attendance-app.mirea.ru/selfapprove?token=lesson-token'
    assert client.post('/site/mark', json={'qr': qr, 'account_ids': [bob, cat]}).status_code == 403
    assert client.post('/site/mark', json={'qr': 'https://evil.example/?token=x', 'account_ids': [bob]}).status_code == 400
    pulse.response = reply(protocol.field(2, protocol.field(1, b'lesson-1')))
    result = client.post('/site/mark', json={'qr': qr, 'account_ids': [ann, bob]}).json()['results']
    assert [r['state'] for r in result] == ['approved', 'approved']
    assert pulse.calls[-1][1] == 'attendance' and b'lesson-token' in pulse.calls[-1][2]
    # A later failure does not hide the approval for this lesson.
    pulse.response = reply(protocol.field(1, protocol.varint(8) + protocol.varint(1)))
    client.post('/site/mark', json={'qr': qr, 'account_ids': [bob]})
    people = {p['label']: p for p in client.get('/site/state').json()['people']}
    assert people['Борис']['mark']['state'] == 'approved'

def test_approval_parsing():
    assert protocol.approval(200, HEADERS, reply(protocol.field(2, b'')).body)[0] == 'approved'
    waiting = reply(protocol.field(1, protocol.varint(8) + protocol.varint(2))).body
    assert protocol.approval(200, HEADERS, waiting) == ('waiting', 'Ожидается подтверждение')
    assert protocol.approval(200, HEADERS, reply(b'', 16).body)[0] == 'expired'
    assert protocol.approval(302, {}, b'')[0] == 'expired'
    assert protocol.attendance_token('https://pulse.mirea.ru/x?token=abc') == 'abc'
    for bad in ('http://pulse.mirea.ru/x?token=abc', 'https://pulse.mirea.ru/x', 'https://u:p@pulse.mirea.ru/?token=a'):
        with pytest.raises(ValueError):
            protocol.attendance_token(bad)

def test_aa_id_hands_over_the_mirea_session(setup):
    client, app, _ = setup
    vault = app.state.service.vault
    body = {'cookie': 'session=new', 'storage_state': {'cookies': []}, 'group_id': 2, 'group_name': 'ГР-2'}
    assert client.post('/internal/aaid/link', json=body).status_code == 401
    assert client.post('/internal/aaid/link', json=body, headers={'authorization': 'Bearer ' + token('new')}).status_code == 401  # app token
    link = token('new', aud='aa-link')
    assert client.post('/internal/aaid/link', json=body, headers={'authorization': 'Bearer ' + token('other', aud='aa-link')}).status_code == 422
    assert client.post('/internal/aaid/link', json=body, headers={'authorization': 'Bearer ' + link}).status_code == 200
    row = vault.account_by_pulse('new')
    assert row['label'] == 'Без имени' and row['group_id'] == 2 and vault.secret(row)['storage_state'] == {'cookies': []}
    # The person names themselves on the AA ID site.
    renamed = client.put('/api/me/account', json={'label': 'Новый Человек'}, headers={'authorization': 'Bearer ' + token('new')})
    assert renamed.json()['label'] == 'Новый Человек'
    # A reconnect keeps the name and group.
    client.post('/internal/aaid/link', json={**body, 'group_id': 3}, headers={'authorization': 'Bearer ' + link})
    row = vault.account_by_pulse('new')
    assert row['label'] == 'Новый Человек' and row['group_id'] == 2

def test_app_marks_count_on_the_site(setup):
    client, app, pulse = setup
    sign_in(client)
    bob = ids(client, ['Борис'])[0]
    pulse.response = reply(protocol.field(2, b''))
    frame = protocol.frame(protocol.field(1, b'token'))
    assert client.post(f'/api/accounts/{bob}/rpc/attendance', content=frame).status_code == 200
    people = {p['label']: p for p in client.get('/site/state').json()['people']}
    assert people['Борис']['mark']['state'] == 'approved'

def test_large_browser_state_is_accepted_only_for_the_link(setup):
    client, app, _ = setup
    big = {'cookie': 'session=big', 'storage_state': {'origins': [{'origin': 'https://pulse.mirea.ru', 'localStorage': [{'name': 'x', 'value': 'y' * 600000}]}]}}
    response = client.post('/internal/aaid/link', json=big, headers={'authorization': 'Bearer ' + token('big', aud='aa-link')})
    assert response.status_code == 200
    assert client.post('/api/accounts/import', json={'cookie': 'session=x', 'label': 'y' * 300000}).status_code == 413

def test_settings_through_aa_id(setup):
    client, app, _ = setup
    bearer = {'authorization': 'Bearer ' + token('ann')}
    assert client.get('/api/me/account').status_code == 401
    me = client.get('/api/me/account', headers=bearer).json()
    assert me == {'connected': True, 'account': True, 'label': 'Анна', 'pass_allowed': False, 'group_name': 'ГР-1'}
    changed = client.put('/api/me/account', json={'pass_allowed': True}, headers=bearer).json()
    assert changed['pass_allowed'] is True and changed['label'] == 'Анна'
    assert app.state.service.vault.account_by_pulse('ann')['pass_allowed'] == 1
    assert client.put('/api/me/account', json={'label': 'x'}, headers={'authorization': 'Bearer ' + token('nobody')}).status_code == 404
    assert client.get('/api/me/account', headers={'authorization': 'Bearer ' + token('nobody')}).json()['account'] is False

def test_username_search_and_pass_list(setup):
    client, app, _ = setup
    vault = app.state.service.vault
    ann_token = {'authorization': 'Bearer ' + token('ann', usr='anna')}
    bob_token = {'authorization': 'Bearer ' + token('bob', usr='boris', sub='user-2')}
    vault.upsert('ann', None, {'cookie': 'session=ann', 'pass_number': '777'})
    ann, bob = vault.account_by_pulse('ann')['id'], vault.account_by_pulse('bob')['id']
    # The username comes from the signed AA ID token.
    assert client.get('/api/me/account', headers=ann_token).status_code == 200
    client.get('/api/me/account', headers=bob_token)
    assert vault.account(ann)['username'] == 'anna'
    found = client.get('/api/me/people?q=@bor', headers=ann_token).json()['people']
    assert found == [{'account_id': bob, 'username': 'boris', 'label': 'Борис', 'group_name': 'ГР-2'}]
    assert client.get('/api/me/people?q=Анна', headers=ann_token).json()['people'] == []  # not yourself
    assert client.get('/api/me/pass', headers=ann_token).json() == {'pass_allowed': False, 'people': []}

    def bob_sees():
        passes = client.get('/api/accounts', headers=bob_token).json()['passes']
        return next(p['number'] for p in passes if p['account_id'] == ann)

    shared = client.post('/api/me/pass/people', json={'account_id': bob}, headers=ann_token).json()
    assert [(p['kind'], p['username']) for p in shared['people']] == [('aaid', 'boris')]
    assert bob_sees() is None  # the pass is still off for everyone
    client.put('/api/me/account', json={'pass_allowed': True}, headers=ann_token)
    assert bob_sees() == '777'
    own = client.get('/api/accounts', headers=ann_token).json()
    assert next(p['number'] for p in own['passes'] if p['account_id'] == ann) == '777'  # the owner, by AA ID
    assert next(a['pass_access'] for a in own['accounts'] if a['id'] == ann) == 'approved'
    cat_token = {'authorization': 'Bearer ' + token('cat', sub='user-3')}
    assert next(p['number'] for p in client.get('/api/accounts', headers=cat_token).json()['passes'] if p['account_id'] == ann) is None
    # One switch blocks everyone on the list; the list stays.
    client.put('/api/me/account', json={'pass_allowed': False}, headers=ann_token)
    assert bob_sees() is None
    assert len(client.get('/api/me/pass', headers=ann_token).json()['people']) == 1
    client.put('/api/me/account', json={'pass_allowed': True}, headers=ann_token)
    assert client.delete(f'/api/me/pass/people/aaid/{bob}', headers=ann_token).json()['people'] == []
    assert bob_sees() is None
    assert client.delete(f'/api/me/pass/people/aaid/{bob}', headers=ann_token).status_code == 404
    assert client.post('/api/me/pass/people', json={'account_id': ann}, headers=ann_token).status_code == 404

def test_telegram_grants_show_in_the_pass_list(setup):
    client, app, _ = setup
    service = app.state.service
    ann = service.vault.account_by_pulse('ann')['id']
    with service.vault.transaction() as db:
        db.execute('INSERT INTO pass_grants VALUES(?,?,?,?,?)', ('g1', ann, 500, 'Телефон Пети', 1))
    bearer = {'authorization': 'Bearer ' + token('ann')}
    people = client.get('/api/me/pass', headers=bearer).json()['people']
    assert [(p['kind'], p['id'], p['label']) for p in people] == [('telegram', 'g1', 'Телефон Пети')]
    assert client.delete('/api/me/pass/people/telegram/g1', headers={'authorization': 'Bearer ' + token('bob')}).status_code == 404
    assert client.delete('/api/me/pass/people/telegram/g1', headers=bearer).json()['people'] == []

def test_sign_in_fills_a_missing_group(setup):
    client, app, _ = setup
    vault = app.state.service.vault
    vault.upsert('fox', 'Фокс', {'cookie': 'session=fox'})
    sign_in(client, 'fox')
    assert vault.account_by_pulse('fox')['group_id'] == 1
    assert 'Фокс' in [p['label'] for p in client.get('/site/state').json()['people']]

def later(minutes_from, minutes_to):
    return ((NOW + timedelta(minutes=minutes_from)).strftime('%Y%m%dT%H%M00'),
            (NOW + timedelta(minutes=minutes_to)).strftime('%Y%m%dT%H%M00'))

def test_break_shows_the_next_lesson_and_own_group_marks_anyway(setup, monkeypatch):
    client, app, pulse = setup
    start, end = later(40, 130)
    if start[:8] != NOW.strftime('%Y%m%d'):
        pytest.skip('too close to midnight')
    monkeypatch.setitem(CALENDARS, 1, calendar('Химия', 'В-1', start=start, end=end))
    sign_in(client)
    state = client.get('/site/state').json()
    assert state['lesson'] is None and state['active'] is None and state['people'] == []
    assert state['next']['subject'] == 'Химия' and state['next']['starts_at'] - time.time() > 35 * 60
    assert state['own_group_size'] == 1
    qr = 'https://pulse.mirea.ru/x?token=t'
    assert client.post('/site/mark', json={'qr': qr, 'account_ids': ['x']}).status_code == 409
    pulse.response = reply(protocol.field(2, b''))
    result = client.post('/site/mark', json={'qr': qr, 'own_group': True}).json()['results']
    assert [(r['label'], r['state']) for r in result] == [('Анна', 'approved')]
    bob = app.state.service.vault.account_by_pulse('bob')['id']
    assert client.post('/site/mark', json={'qr': qr, 'own_group': True, 'account_ids': [bob]}).status_code == 403

def test_people_shown_shortly_before_the_lesson(setup, monkeypatch):
    client, _, _ = setup
    start, end = later(10, 100)
    if start[:8] != NOW.strftime('%Y%m%d'):
        pytest.skip('too close to midnight')
    for group in (1, 2):
        monkeypatch.setitem(CALENDARS, group, calendar('Химия', 'В-1', start=start, end=end))
    sign_in(client)
    state = client.get('/site/state').json()
    assert state['lesson'] is None and state['active'] == 'soon'
    assert {p['label'] for p in state['people']} == {'Анна', 'Борис'}

def test_calendars_parse_once_and_back_off_after_a_failure(monkeypatch):
    import asyncio
    calls, parses = [], []
    answers = [None, calendar('Физика', 'А-101')]
    async def fetch(url):
        calls.append(url)
        return answers.pop(0) if answers else calendar('Физика', 'А-101')
    real = schedule.lessons_on
    monkeypatch.setattr(schedule, 'lessons_on', lambda text, day: parses.append(day) or real(text, day))
    calendars = schedule.Calendars(fetch)
    day = NOW.date()
    async def run():
        assert await calendars.lessons(1, day) is None
        assert await calendars.lessons(1, day) is None       # no retry within a minute
        calendars.failed[1] -= 61
        first = await calendars.lessons(1, day)
        first[0]['subject'] = 'changed'
        again = await calendars.lessons(1, day)
        return first, again
    first, again = asyncio.run(run())
    assert len(calls) == 2 and len(parses) == 1 and again[0]['subject'] == 'Физика'

def test_slow_pulse_does_not_hold_the_lesson_page(setup, monkeypatch):
    import asyncio
    from app import site
    client, app, pulse = setup
    monkeypatch.setattr(site, 'INSIDE_WAIT', 0.2)
    async def slow(cookie, uid, start, end):
        await asyncio.sleep(3)
        return [], True
    pulse.acs_timeline = slow
    sign_in(client)
    started = time.time()
    people = client.get('/site/state').json()['people']
    assert time.time() - started < 2 and all(p['inside'] is None for p in people)

def test_today_shows_every_lesson_with_marks_of_the_groups_that_were_there(setup):
    client, app, pulse = setup
    vault = app.state.service.vault
    sign_in(client)
    bob = next(row for row in vault.accounts() if row['label'] == 'Борис')
    vault.record_mark(bob['id'], 'rejected', 'нет', 'site')
    vault.record_mark(bob['id'], 'approved', '', 'site')
    vault.record_mark(bob['id'], 'error', 'later', 'site')
    data = client.get('/site/today').json()
    assert data['group_name'] == 'ГР-1' and len(data['lessons']) == 1
    lesson = data['lessons'][0]
    assert lesson['subject'] == 'Физика' and lesson['state'] == 'now'
    assert [g['id'] for g in lesson['groups']] == [1, 2]
    labels = {p['id']: p['label'] for p in data['people']}
    assert sorted(labels[i] for i in lesson['marks']) == ['Анна', 'Борис']
    assert lesson['marks'][bob['id']]['state'] == 'approved'
    assert [p['label'] for p in data['people']] == ['Анна', 'Борис']
    assert client.get('/today').status_code == 200

def test_today_needs_sign_in(setup):
    assert setup[0].get('/site/today').status_code == 401

def test_mirea_journal_status_shows_on_the_lesson_and_today(setup):
    client, app, pulse = setup
    sign_in(client)
    people = {p['label']: p for p in client.get('/site/state').json()['people']}
    assert people['Борис']['mirea'] == 'present' and people['Анна']['mirea'] == 'absent'
    today = client.get('/site/today').json()
    lesson = today['lessons'][0]
    by_label = {p['label']: p['id'] for p in today['people']}
    assert lesson['mirea'][by_label['Борис']] == 'present' and lesson['mirea'][by_label['Анна']] == 'absent'
    reads = pulse.journal_reads
    client.get('/site/state')
    assert pulse.journal_reads == reads          # cached for MIREA_TTL

def test_pulse_journal_parsers():
    from app.protocol import field, frame, _vfield, visiting_logs, day_lessons, lesson_attendance, day_lessons_request
    headers = {'content-type': 'application/grpc-web+proto', 'grpc-status': '0'}
    stamp = lambda n, t: field(n, _vfield(1, t))
    journal = field(1, b'J1') + field(2, 'ГР-1'.encode()) + field(6, field(2, 'Осень'.encode()) + stamp(3, 100) + stamp(4, 200))
    logs = frame(field(2, field(1, field(3, b'25X') ) + field(2, journal) + _vfield(3, 1)))
    assert visiting_logs(logs, headers) == [{'id': 'J1', 'group': 'ГР-1', 'start': 100, 'end': 200}]
    lesson = field(1, b'L1') + stamp(2, 1000) + stamp(3, 6400) + field(4, field(1, b'd') + field(2, 'Физика'.encode()))
    lessons = frame(field(2, field(3, lesson) + field(4, field(1, b''))))
    assert day_lessons(lessons, headers) == [{'id': 'L1', 'start': 1000, 'end': 6400, 'subject': 'Физика'}]
    student = lambda human, word: field(1, field(1, b's') + field(9, human.encode()) + field(7, field(1, f'x|1|{word}'.encode()) + field(7, field(1, b''))))
    attendance = frame(student('h1', 'Present') + student('h2', 'Absent') + student('h3', 'ExcusedAbsence'))
    assert lesson_attendance(attendance, headers) == {'h1': 'present', 'h2': 'absent', 'h3': 'excused'}
    assert lesson_attendance(frame(b''), headers) == {}
    assert visiting_logs(frame(b''), {**headers, 'grpc-status': '16'}) is None
    from datetime import date
    assert day_lessons_request(date(2026, 10, 27)) == bytes.fromhex('0000000009120708ea0f100a181b')

def test_telegram_link_is_made_by_the_signed_in_person(setup):
    from types import SimpleNamespace
    client, app, pulse = setup
    assert client.post('/site/telegram-link').status_code == 401
    sign_in(client)
    service = app.state.service
    service.telegram = None
    assert client.post('/site/telegram-link').status_code == 503
    service.telegram = SimpleNamespace(username='aa_test_bot')
    url = client.post('/site/telegram-link').json()['url']
    assert url.startswith('https://t.me/aa_test_bot?start=aaid_')
    claimed = service.vault.claim_telegram_code(url.split('aaid_')[1])
    assert claimed['account_id'] == service.vault.account_by_pulse('ann')['id']
    assert client.get('/tg/connect').status_code == 200

def test_sign_in_returns_to_the_page_it_was_asked_from(setup):
    client = setup[0]
    start = client.get('/auth/aaid?next=/tg/connect', follow_redirects=False)
    state = start.headers['location'].split('state=')[1]
    done = client.get(f'/auth/callback?code=ann{"x" * 17}&state={state}', follow_redirects=False)
    assert done.headers['location'] == '/tg/connect'
    start = client.get('/auth/aaid?next=https://evil.example', follow_redirects=False)
    state = start.headers['location'].split('state=')[1]
    assert client.get(f'/auth/callback?code=ann{"x" * 17}&state={state}', follow_redirects=False).headers['location'] == '/'

def test_week_schedule_page_for_own_and_other_groups(setup):
    client, app, pulse = setup
    assert client.get('/site/schedule').status_code == 401
    sign_in(client)
    data = client.get('/site/schedule').json()
    assert data['group_id'] == 1 and data['own'] and len(data['days']) == 7
    today = next(day for day in data['days'] if day['today'])
    assert [l['subject'] for l in today['lessons']] == ['Физика'] and today['lessons'][0]['state'] == 'now'
    other = client.get('/site/schedule?group=3').json()
    assert not other['own'] and other['group_name'] == 'ГР-3'
    later = client.get('/site/schedule?week=1').json()
    assert later['monday'] > data['monday'] and not any(day['lessons'] for day in later['days'])
    assert client.get('/site/state').json()['schedule_url'] == '/schedule'
    assert client.get('/schedule').status_code == 200
