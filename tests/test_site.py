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
    async def acs_timeline(self, cookie, uid, start, end):
        return [], uid == 'ann'

class Link(Verifier):
    async def exchange(self, code):
        return {'id': 'aaid-' + code[:3], 'mirea': code[:3], 'group_id': None, 'group_name': None} if code.endswith('x' * 17) else None

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
    # The person names themselves on the site.
    sign_in(client, 'new')
    assert client.put('/site/me/name', json={'label': 'Новый Человек'}).json()['label'] == 'Новый Человек'
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
